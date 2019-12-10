FROM golang:1.13.5-alpine3.10 as build-env

RUN mkdir /podnodselector-webhook
WORKDIR /podnodeselector-webhook

ENV GO111MODULE=on

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .


RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -o /go/bin/podnodeselector-webhook




FROM alpine:3.10.3
RUN apk add --update --no-cache ca-certificates git tzdata
COPY --from=build-env /go/bin/podnodeselector-webhook /go/bin/podnodeselector-webhook
ENTRYPOINT ["/go/bin/podnodeselector-webhook"]