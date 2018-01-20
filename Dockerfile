FROM golang:alpine

ENV DOCKER_API_VERSION=1.35

ADD cbuild /

CMD ["/cbuild"]