FROM golang:buster

RUN apt-get update && apt-get install -y --no-install-recommends \
    glusterfs-server \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /go/src/app
COPY . .

RUN go get -d -v ./
RUN go install -v ./
