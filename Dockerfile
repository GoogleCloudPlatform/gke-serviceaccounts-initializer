# build
FROM golang:1.8
WORKDIR /go/src/
COPY . ./
RUN go get -d -v ./...
RUN CGO_ENABLED=0 GOOS=linux go install -a -installsuffix cgo ./cmd/gke-serviceaccounts-initializer

# package
FROM alpine:latest  
RUN apk --no-cache add ca-certificates && rm -rf /var/cache/apk/* /tmp/*
COPY --from=0 /go/bin/gke-serviceaccounts-initializer /bin/
CMD ["/bin/gke-serviceaccounts-initializer"]
