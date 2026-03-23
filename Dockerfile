FROM golang:1.22-alpine

WORKDIR /

COPY . /

RUN go build -o proxy main.go
RUN go build -o backend backend.go

EXPOSE 8080 8081 9000 9001 9002

CMD ["./proxy"]