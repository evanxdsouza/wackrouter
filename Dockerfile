FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /wackrouter .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /wackrouter /wackrouter
ENV WACKROUTER_DATA=/data
VOLUME /data
EXPOSE 8787
ENTRYPOINT ["/wackrouter"]
