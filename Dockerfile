FROM alpine:3.19 AS certs
RUN addgroup -g 10001 user && \
    adduser -H -D -u 10000 -s /bin/false -G user user && \
    grep user /etc/passwd > /etc/passwd_user && grep user /etc/group > /etc/group_user
RUN apk add --no-cache ca-certificates && update-ca-certificates

FROM scratch
ENTRYPOINT ["/scheduler"]
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=certs /etc/passwd_user /etc/passwd
COPY --from=certs /etc/group_user /etc/group
COPY scheduler /
USER user:user