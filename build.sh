#!/bin/sh

#Compile into static binary and cross-compile for linux
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o cbuild .

#Build the container
docker build --force-rm -t cbuild .