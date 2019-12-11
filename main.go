package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog"
)

func health(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func main() {
	var parameters WhSvrParameters

	// get command line parameters
	flag.IntVar(&parameters.port, "port", 4443, "Webhook server port.")
	flag.StringVar(&parameters.certFile, "tlsCertFile", "", "File containing the x509 Certificate for HTTPS.")
	flag.StringVar(&parameters.keyFile, "tlsKeyFile", "", "File containing the x509 private key to --tlsCertFile.")
	flag.BoolVar(&parameters.inCluster, "inCluster", false, "Running in cluster")
	flag.Parse()

	listenAddr := fmt.Sprintf(":%v", parameters.port)

	whsvr := &WebhookServer{
		server: &http.Server{
			Addr: listenAddr,
		},
	}

	kubeClientSet, kubeClientSetErr := KubeClientSet(parameters.inCluster)

	if kubeClientSetErr != nil {
		klog.Fatal(kubeClientSetErr)
	}

	whsvr.SetExternalKubeClientSet(kubeClientSet)

	err := whsvr.ValidateInitialization()

	if err != nil {
		klog.Fatal(err)
	}

	// define http server and server handler
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", whsvr.serve)
	mux.HandleFunc("/validate", whsvr.serve)
	mux.HandleFunc("/health", health)
	whsvr.server.Handler = mux

	// start webhook server in new routine
	go func() {
		var err error
		if parameters.certFile != "" && parameters.keyFile != "" {
			err = whsvr.server.ListenAndServeTLS(parameters.certFile, parameters.keyFile)
		} else {
			err = whsvr.server.ListenAndServe()
		}

		if err != nil {
			klog.Errorf("Failed to listen and serve webhook server: %v", err)
		}

	}()

	klog.Infof("Server started on %s", listenAddr)

	// listening OS shutdown singal
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan

	klog.Infof("Got OS shutdown signal, shutting down webhook server gracefully...")
	whsvr.server.Shutdown(context.Background())
}
