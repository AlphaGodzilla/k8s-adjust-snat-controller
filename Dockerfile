FROM golang:1.22.5-alpine as builder
WORKDIR /k8s-adjust-snat-controller
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build

FROM alpine:3.17.2
COPY --from=builder /k8s-adjust-snat-controller/k8s-adjust-snat-controller /
RUN apk --no-cache add ca-certificates && update-ca-certificates
CMD ["/k8s-adjust-snat-controller"]