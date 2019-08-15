FROM golang
WORKDIR /go/src/build-helper
RUN curl https://glide.sh/get | sh
COPY ./glide.yaml ./glide.lock ./
RUN glide install
COPY . .
RUN GOOS=linux GOARCH=amd64 go build -v -o /go/bin/build
ENTRYPOINT [ "/go/bin/app" ]

FROM ubuntu:bionic
RUN apt-get update && apt-get install -y ca-certificates
COPY --from=0 /go/bin/build /usr/bin/build
RUN chmod +x /usr/bin/build
CMD [ "sh", "-c" ]