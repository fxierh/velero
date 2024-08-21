/*
Copyright The Velero Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package datapath

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/pkg/uploader"
	"github.com/vmware-tanzu/velero/pkg/util/kube"

	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/vmware-tanzu/velero/pkg/util/logging"
)

const (
	TaskTypeBackup  = "backup"
	TaskTypeRestore = "restore"

	ErrCancelled = "data path is canceled"

	EventReasonStarted    = "Data-Path-Started"
	EventReasonCompleted  = "Data-Path-Completed"
	EventReasonFailed     = "Data-Path-Failed"
	EventReasonCancelled  = "Data-Path-Canceled"
	EventReasonProgress   = "Data-Path-Progress"
	EventReasonCancelling = "Data-Path-Canceling"
)

type microServiceBRWatcher struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	log                 logrus.FieldLogger
	client              client.Client
	kubeClient          kubernetes.Interface
	mgr                 manager.Manager
	namespace           string
	callbacks           Callbacks
	taskName            string
	taskType            string
	thisPod             string
	thisContainer       string
	associatedObject    string
	eventCh             chan *v1.Event
	podCh               chan *v1.Pod
	startedFromEvent    bool
	terminatedFromEvent bool
	wgWatcher           sync.WaitGroup
	eventInformer       ctrlcache.Informer
	podInformer         ctrlcache.Informer
	eventHandler        cache.ResourceEventHandlerRegistration
	podHandler          cache.ResourceEventHandlerRegistration
	watcherLock         sync.Mutex
}

func newMicroServiceBRWatcher(client client.Client, kubeClient kubernetes.Interface, mgr manager.Manager, taskType string, taskName string, namespace string,
	podName string, containerName string, associatedObject string, callbacks Callbacks, log logrus.FieldLogger) AsyncBR {
	ms := &microServiceBRWatcher{
		mgr:              mgr,
		client:           client,
		kubeClient:       kubeClient,
		namespace:        namespace,
		callbacks:        callbacks,
		taskType:         taskType,
		taskName:         taskName,
		thisPod:          podName,
		thisContainer:    containerName,
		associatedObject: associatedObject,
		eventCh:          make(chan *v1.Event, 10),
		podCh:            make(chan *v1.Pod, 2),
		wgWatcher:        sync.WaitGroup{},
		log:              log,
	}

	return ms
}

func (ms *microServiceBRWatcher) Init(ctx context.Context, param interface{}) error {
	eventInformer, err := ms.mgr.GetCache().GetInformer(ctx, &v1.Event{})
	if err != nil {
		return errors.Wrap(err, "error getting event informer")
	}

	podInformer, err := ms.mgr.GetCache().GetInformer(ctx, &v1.Pod{})
	if err != nil {
		return errors.Wrap(err, "error getting pod informer")
	}

	eventHandler, err := eventInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				evt := obj.(*v1.Event)
				if evt.InvolvedObject.Namespace != ms.namespace || evt.InvolvedObject.Name != ms.associatedObject {
					return
				}

				ms.eventCh <- evt
			},
			UpdateFunc: func(_, obj interface{}) {
				evt := obj.(*v1.Event)
				if evt.InvolvedObject.Namespace != ms.namespace || evt.InvolvedObject.Name != ms.associatedObject {
					return
				}

				ms.eventCh <- evt
			},
		},
	)
	if err != nil {
		return errors.Wrap(err, "error registering event handler")
	}

	podHandler, err := podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, obj interface{}) {
				pod := obj.(*v1.Pod)
				if pod.Namespace != ms.namespace || pod.Name != ms.thisPod {
					return
				}

				if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
					ms.podCh <- pod
				}
			},
		},
	)
	if err != nil {
		return errors.Wrap(err, "error registering pod handler")
	}

	if err := ms.reEnsureThisPod(ctx); err != nil {
		return err
	}

	ms.eventInformer = eventInformer
	ms.podInformer = podInformer
	ms.eventHandler = eventHandler
	ms.podHandler = podHandler

	ms.ctx, ms.cancel = context.WithCancel(ctx)

	ms.log.WithFields(
		logrus.Fields{
			"taskType": ms.taskType,
			"taskName": ms.taskName,
			"thisPod":  ms.thisPod,
		}).Info("MicroServiceBR is initialized")

	return nil
}

func (ms *microServiceBRWatcher) Close(ctx context.Context) {
	if ms.cancel != nil {
		ms.cancel()
	}

	ms.log.WithField("taskType", ms.taskType).WithField("taskName", ms.taskName).Info("Closing MicroServiceBR")

	ms.wgWatcher.Wait()

	ms.close()

	ms.log.WithField("taskType", ms.taskType).WithField("taskName", ms.taskName).Info("MicroServiceBR is closed")
}

func (ms *microServiceBRWatcher) close() {
	ms.watcherLock.Lock()
	defer ms.watcherLock.Unlock()

	if ms.eventHandler != nil {
		if err := ms.eventInformer.RemoveEventHandler(ms.eventHandler); err != nil {
			ms.log.WithError(err).Warn("Failed to remove event handler")
		}

		ms.eventHandler = nil
	}

	if ms.podHandler != nil {
		if err := ms.podInformer.RemoveEventHandler(ms.podHandler); err != nil {
			ms.log.WithError(err).Warn("Failed to remove pod handler")
		}

		ms.podHandler = nil
	}
}

func (ms *microServiceBRWatcher) StartBackup(source AccessPoint, uploaderConfig map[string]string, param interface{}) error {
	ms.log.Infof("Start watching backup ms for source %v", source.ByPath)

	ms.startWatch()

	return nil
}

func (ms *microServiceBRWatcher) StartRestore(snapshotID string, target AccessPoint, uploaderConfigs map[string]string) error {
	ms.log.Infof("Start watching restore ms to target %s, from snapshot %s", target.ByPath, snapshotID)

	ms.startWatch()

	return nil
}

func (ms *microServiceBRWatcher) reEnsureThisPod(ctx context.Context) error {
	thisPod := &v1.Pod{}
	if err := ms.client.Get(ctx, types.NamespacedName{
		Namespace: ms.namespace,
		Name:      ms.thisPod,
	}, thisPod); err != nil {
		return errors.Wrapf(err, "error getting this pod %s", ms.thisPod)
	}

	if thisPod.Status.Phase == v1.PodSucceeded || thisPod.Status.Phase == v1.PodFailed {
		ms.podCh <- thisPod
		ms.log.WithField("this pod", ms.thisPod).Infof("This pod comes to terminital status %s before watch start", thisPod.Status.Phase)
	}

	return nil
}

var funcGetPodTerminationMessage = kube.GetPodContainerTerminateMessage
var funcRedirectLog = redirectDataMoverLogs
var funcGetResultFromMessage = getResultFromMessage
var funcGetProgressFromMessage = getProgressFromMessage

var eventWaitTimeout time.Duration = time.Minute

func (ms *microServiceBRWatcher) startWatch() {
	ms.wgWatcher.Add(1)

	go func() {
		ms.log.Info("Start watching data path pod")

		defer func() {
			ms.close()
			ms.wgWatcher.Done()
		}()

		var lastPod *v1.Pod

	watchLoop:
		for {
			select {
			case <-ms.ctx.Done():
				break watchLoop
			case pod := <-ms.podCh:
				lastPod = pod
				break watchLoop
			case evt := <-ms.eventCh:
				ms.onEvent(evt)
			}
		}

		if lastPod == nil {
			ms.log.Warn("Watch loop is canceled on waiting data path pod")
			return
		}

	epilogLoop:
		for !ms.startedFromEvent || !ms.terminatedFromEvent {
			select {
			case <-ms.ctx.Done():
				ms.log.Warn("Watch loop is canceled on waiting final event")
				return
			case <-time.After(eventWaitTimeout):
				break epilogLoop
			case evt := <-ms.eventCh:
				ms.onEvent(evt)
			}
		}

		terminateMessage := funcGetPodTerminationMessage(lastPod, ms.thisContainer)

		logger := ms.log.WithField("data path pod", lastPod.Name)

		logger.Infof("Finish waiting data path pod, phase %s, message %s", lastPod.Status.Phase, terminateMessage)

		if !ms.startedFromEvent {
			logger.Warn("VGDP seems not started")
		}

		if ms.startedFromEvent && !ms.terminatedFromEvent {
			logger.Warn("VGDP started but termination event is not received")
		}

		logger.Info("Recording data path pod logs")

		if err := funcRedirectLog(ms.ctx, ms.kubeClient, ms.namespace, lastPod.Name, ms.thisContainer, ms.log); err != nil {
			logger.WithError(err).Warn("Failed to collect data mover logs")
		}

		logger.Info("Calling callback on data path pod termination")

		if lastPod.Status.Phase == v1.PodSucceeded {
			ms.callbacks.OnCompleted(ms.ctx, ms.namespace, ms.taskName, funcGetResultFromMessage(ms.taskType, terminateMessage, ms.log))
		} else {
			if terminateMessage == ErrCancelled {
				ms.callbacks.OnCancelled(ms.ctx, ms.namespace, ms.taskName)
			} else {
				ms.callbacks.OnFailed(ms.ctx, ms.namespace, ms.taskName, errors.New(terminateMessage))
			}
		}

		logger.Info("Complete callback on data path pod termination")
	}()
}

func (ms *microServiceBRWatcher) onEvent(evt *v1.Event) {
	switch evt.Reason {
	case EventReasonStarted:
		ms.startedFromEvent = true
		ms.log.Infof("Received data path start message: %s", evt.Message)
	case EventReasonProgress:
		ms.callbacks.OnProgress(ms.ctx, ms.namespace, ms.taskName, funcGetProgressFromMessage(evt.Message, ms.log))
	case EventReasonCompleted:
		ms.log.Infof("Received data path completed message: %v", funcGetResultFromMessage(ms.taskType, evt.Message, ms.log))
		ms.terminatedFromEvent = true
	case EventReasonCancelled:
		ms.log.Infof("Received data path canceled message: %s", evt.Message)
		ms.terminatedFromEvent = true
	case EventReasonFailed:
		ms.log.Infof("Received data path failed message: %s", evt.Message)
		ms.terminatedFromEvent = true
	case EventReasonCancelling:
		ms.log.Infof("Received data path canceling message: %s", evt.Message)
	default:
		ms.log.Infof("Received event for data path %s, reason: %s, message: %s", ms.taskName, evt.Reason, evt.Message)
	}
}

func getResultFromMessage(taskType string, message string, logger logrus.FieldLogger) Result {
	result := Result{}

	if taskType == TaskTypeBackup {
		backupResult := BackupResult{}
		err := json.Unmarshal([]byte(message), &backupResult)
		if err != nil {
			logger.WithError(err).Errorf("Failed to unmarshal result message %s", message)
		} else {
			result.Backup = backupResult
		}
	} else {
		restoreResult := RestoreResult{}
		err := json.Unmarshal([]byte(message), &restoreResult)
		if err != nil {
			logger.WithError(err).Errorf("Failed to unmarshal result message %s", message)
		} else {
			result.Restore = restoreResult
		}
	}

	return result
}

func getProgressFromMessage(message string, logger logrus.FieldLogger) *uploader.Progress {
	progress := &uploader.Progress{}
	err := json.Unmarshal([]byte(message), progress)
	if err != nil {
		logger.WithError(err).Debugf("Failed to unmarshal progress message %s", message)
	}

	return progress
}

func (ms *microServiceBRWatcher) Cancel() {
	ms.log.WithField("taskType", ms.taskType).WithField("taskName", ms.taskName).Info("MicroServiceBR is canceled")
}

var funcCreateTemp = os.CreateTemp
var funcCollectPodLogs = kube.CollectPodLogs

func redirectDataMoverLogs(ctx context.Context, kubeClient kubernetes.Interface, namespace string, thisPod string, thisContainer string, logger logrus.FieldLogger) error {
	logger.Infof("Starting to collect data mover pod log for %s", thisPod)

	logFile, err := funcCreateTemp("", "")
	if err != nil {
		return errors.Wrap(err, "error to create temp file for data mover pod log")
	}

	defer logFile.Close()

	logFileName := logFile.Name()
	logger.Infof("Created log file %s", logFileName)

	err = funcCollectPodLogs(ctx, kubeClient.CoreV1(), thisPod, namespace, thisContainer, logFile)
	if err != nil {
		return errors.Wrapf(err, "error to collect logs to %s for data mover pod %s", logFileName, thisPod)
	}

	logFile.Close()

	logger.Infof("Redirecting to log file %s", logFileName)

	hookLogger := logger.WithField(logging.LogSourceKey, logFileName)
	hookLogger.Logln(logging.ListeningLevel, logging.ListeningMessage)

	logger.Infof("Completed to collect data mover pod log for %s", thisPod)

	return nil
}
