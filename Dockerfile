# build
FROM golang:1.8
WORKDIR /go/src/github.com/ahmetb/gcp-serviceaccounts-initializer
COPY main.go .
RUN go get -d -v ./...
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o gcp-serviceaccounts-initializer .

# package
FROM alpine:latest  
RUN apk --no-cache add ca-certificates && rm -rf /var/cache/apk/* /tmp/*
COPY --from=0 /go/src/github.com/ahmetb/gcp-serviceaccounts-initializer /bin/
CMD ["/bin/gcp-serviceaccounts-initializer"]
