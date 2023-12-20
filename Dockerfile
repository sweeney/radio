# syntax=docker/dockerfile:1

FROM golang:1.21

WORKDIR /usr/src/app

COPY go.mod ./
COPY *.go ./
COPY ./tracks ./tracks


RUN go mod download && go mod verify
RUN CGO_ENABLED=0 GOOS=linux go build -o /radio

EXPOSE 3000
CMD ["/radio"]
