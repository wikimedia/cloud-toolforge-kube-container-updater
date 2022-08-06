# kube-container-updater

kube-container-updater is a program that will locate Kubernetes pods
with 'latest' container references that are running outdated versions
of those containers and restart them.

It is written in Go and designed to be deployed as a CronJob inside a
Kubernetes cluster.
