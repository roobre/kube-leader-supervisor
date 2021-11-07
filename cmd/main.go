package main

import (
	"os"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"roob.re/kls"
)

func main() {
	conf, err := clientcmd.BuildConfigFromFlags("", "/home/roobre/.kube/config")
	if err != nil {
		log.Fatal(err)
	}

	cs, err := kubernetes.NewForConfig(conf)
	if err != nil {
		log.Fatal(err)
	}

	supervisor, err := kls.New(os.Args[1:], kls.WithKubeClient(cs), kls.WithLeaseTiming(kls.DefaultLeaseTimings))
	if err != nil {
		log.Fatal(err)
	}

	supervisor.Run()
}
