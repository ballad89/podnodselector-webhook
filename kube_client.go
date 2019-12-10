package main

import (
	"errors"

	pathutil "github.com/JaSei/pathutil-go"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func KubeClientSet(inCluster bool) (*kubernetes.Clientset, error) {
	var config *rest.Config

	if inCluster {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		config = c
	} else {
		kubeconfig, err := pathutil.Home(".kube", "config")
		if err != nil {
			return nil, err
		}

		if kubeconfig.IsFile() {
			c, err := clientcmd.BuildConfigFromFlags("", kubeconfig.String())
			if err != nil {
				return nil, err
			}
			config = c
		} else {
			return nil, errors.New(kubeconfig.String() + " does not exist")
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}
