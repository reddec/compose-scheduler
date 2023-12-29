FROM alpine:3.19 AS certs
RUN apk add --no-cache ca-certificates && update-ca-certificates

FROM scratch
ENTRYPOINT ["/scheduler"]
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY scheduler /