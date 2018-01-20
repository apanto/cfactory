#!/bin/sh 

docker run -it --rm \
-v ~/.aws/:/root/.aws \
-v /var/run/docker.sock:/var/run/docker.sock \
-e DOCKER_API_VERSION=1.35 cbuild \
/cbuild -r github.com/apanto/cfactory.git#:testc