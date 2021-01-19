package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	v1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	tektoncdclientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
)

const (
	//SINGALPATH is path for signal(file)
	SINGALPATH = "/termination"
	//ACTIVITYPATH is the folder which will list all running Activities
	ACTIVITYPATH = "/jobActivities"
	//STOPANDWAIT is signal for send stop request and wait for stop success
	STOPANDWAIT = "stop_and_wait"
	//STOP is the signal for only send stop request, not wait for response
	STOP = "stop"
	//ABORTANDWAIT is signnal for not send stop request and wait for job runningfinished
	ABORTANDWAIT = "abort_and_wait"
	//ABORT is signal for not send stop request and not wait for job running finished
	ABORT = "abort"
	//PREFIXRUNNING is the prefix of signal means a job activity is runninng
	PREFIXRUNNING = "runing-"
	//PREFIXFINISHED is the prefix of signal means a job is finished
	PREFIXFINISHED = "finished-"
)

func main() {
	var namespace string
	var name string
	var sendStop bool
	var waitforFinish bool
	var monitorInterval string
	var monitorTimeout string

	flag.StringVar(&namespace, "namespace", "", "The namespace of the pipelinerun.")
	flag.StringVar(&name, "name", "", "The name of the pipelinerun.")
	flag.BoolVar(&sendStop, "send-stop", true, "Send stop request to running job or not.")
	flag.BoolVar(&waitforFinish, "wait", true, "Wait running job finished or wait for stop response or not.")
	flag.StringVar(&monitorInterval, "monitor-interval", "2", "Monitor interval(second) when watch the singal response from Job Activity tasks")
	flag.StringVar(&monitorTimeout, "monitor-timeout", "60", "Monitor timeout(second) when no singal response from Job Activity tasks")
	flag.Parse()

	// prepare tekton client
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Errorf("Get config of the cluster failed: %+v", err)
		os.Exit(1)
	}

	tektonClient, err := tektoncdclientset.NewForConfig(config)
	if err != nil {
		log.Errorf("Get client of tekton failed: %+v", err)
		os.Exit(1)
	}

	//pause the pipeline, waiting for https://github.com/tektoncd/pipeline/pull/3522 merged
	// err = pausePipelinerun(tektonClient, namespace, name)
	// if err != nil {
	// 	log.Errorf("Pause pipelinerun failed: %+v:", err)
	// 	os.Exit(1)
	// }

	//if no send stop signal and no wait, the cancel the pipeline immediatly
	if !sendStop && !waitforFinish {
		err = cancelPipelinerun(tektonClient, namespace, name)
		if err != nil {
			log.Errorf("Cancel pipelinerun failed: %+v:", err)
			os.Exit(1)
		}

		os.Exit(0)
	}

	//Check if aother Terminator is already running and create signal path
	signalPath := fmt.Sprintf("%s/%s/%s", SINGALPATH, namespace, name)
	_, err = os.Stat(signalPath)
	if err == nil {
		//already existed
		log.Infof("There is another Terminator working: %+v", err)
		os.Exit(0)
	}

	if !os.IsNotExist(err) {
		// unexpected error happen
		log.Errorf("Detect signal path hit error: %+v", err)
		os.Exit(1)
	}

	err = os.MkdirAll(signalPath+ACTIVITYPATH, 0777)
	if err != nil {
		if os.IsExist(err) {
			//already existed
			log.Infof("There is another Terminator working: %+v", err)
			os.Exit(0)
		}
		log.Errorf("Create signal path hit error: %+v", err)
		os.Exit(1)
	}

	defer os.RemoveAll(signalPath)

	//touch singal files
	if sendStop && waitforFinish {
		err = sendSingal(signalPath, STOPANDWAIT)
	} else if sendStop {
		err = sendSingal(signalPath, STOP)
	} else if waitforFinish {
		err = sendSingal(signalPath, ABORTANDWAIT)
	}

	if err != nil {
		if os.IsExist(err) {
			log.Errorf("Signal already existed, another Terminator is running, exit: %+v:", err)
			os.Exit(0)
		}

		log.Errorf("Failed to send signal, hit error: %+v:", err)
		os.Exit(1)
	}

	// Start folder watcher using PollImmediateInfinite
	monitorIntervalI, err := strconv.Atoi(monitorInterval)
	if err != nil {
		log.Errorf("Parse parameter failed: %+v:", err)
		os.Exit(1)
	}
	monitorTimeoutI, err := strconv.Atoi(monitorTimeout)
	if err != nil {
		log.Errorf("Parse parameter failed: %+v:", err)
		os.Exit(1)
	}

	activitiesPath := signalPath + ACTIVITYPATH
	err = checkResponseSignal(monitorIntervalI, monitorTimeoutI, activitiesPath)
	if err != nil {
		log.Errorf("Check response failed: %+v:", err)
		os.Exit(1)
	}

	//cancel the pipelinerun
	//fmt.Println("should cancel, but not")
	
	err = cancelPipelinerun(tektonClient, namespace, name)
	if err != nil {
		log.Errorf("Cancel pipelinerun failed: %+v:", err)
		os.Exit(1)
	}
	
	//os.Exit(0)
}

func sendSingal(signalPath string, signal string) error {
	path := fmt.Sprintf("%s/%s", signalPath, signal)
	file, err := os.Create(path)
	defer file.Close()

	if err != nil {
		if os.IsExist(err) {
			log.Infof("There is another Terminator working: %+v", err)
			return os.ErrExist
		}
		return err
	}

	return nil
}

func checkResponseSignal(monitorIntervalI int, timeoutSec int, activitiesPath string) error {
	duration := time.Duration(monitorIntervalI)
	startTime := time.Now()

	err := wait.PollImmediateInfinite(time.Second*duration,
		func() (bool, error) {
			noActivities, allFinish, err := checkActivitiesPath(activitiesPath)
			if err == nil {
				if noActivities {
					lastedTime := time.Since(startTime)
					if lastedTime.Seconds() > float64(timeoutSec) {
						return true, nil
					}
				} else if allFinish {
					return true, nil
				}

				return false, nil
			}

			return false, err
		})

	if err != nil {
		return err
	}

	return nil
}

func checkActivitiesPath(activitiesPath string) (bool, bool, error) {
	noActivities := true
	allFinish := true

	err := filepath.Walk(activitiesPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			fmt.Printf("prevent panic by handling failure accessing a path %q: %v\n", path, err)
			return err
		}

		if !strings.HasPrefix(info.Name(), PREFIXRUNNING) && !strings.HasPrefix(info.Name(), PREFIXFINISHED) {
			return nil
		}

		noActivities = false

		if strings.HasPrefix(info.Name(), PREFIXRUNNING) {
			fmt.Printf("find response signal: %+v \n", info.Name())
			allFinish = false
			return nil
		}

		return nil
	})

	if err != nil {
		return noActivities, allFinish, err
	}

	return noActivities, allFinish, nil
}

func cancelPipelinerun(client tektoncdclientset.Interface, namespace, name string) error {
	pr, err := client.TektonV1beta1().PipelineRuns(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Get Pipelinerun: %s failed: %+v", namespace+"/"+name, err)
		os.Exit(1)
	}

	pr.Spec.Status = v1beta1.PipelineRunSpecStatusCancelled
	_, err = client.TektonV1beta1().PipelineRuns(namespace).Update(context.TODO(), pr, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	return nil
}

func pausePipelinerun(client tektoncdclientset.Interface, namespace, name string) error {
	pr, err := client.TektonV1beta1().PipelineRuns(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Get Pipelinerun: %s failed: %+v", namespace+"/"+name, err)
		os.Exit(1)
	}

	pr.Spec.Status = "PipelineRunPending"
	_, err = client.TektonV1beta1().PipelineRuns(namespace).Update(context.TODO(), pr, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	return nil
}
