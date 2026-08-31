FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN go build -o /out/omnigate2api ./cmd/server && \
    go build -o /out/omnigate2api-login ./cmd/login && \
    go build -o /out/omnigate2api-login-tencent ./cmd/login-tencent && \
    go build -o /out/omnigate2api-credit ./cmd/credit && \
    go build -o /out/omnigate2api-apply ./cmd/apply

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY config.example.json ./config.example.json
VOLUME ["/app/auths", "/app/data"]
EXPOSE 7866
ENV OMNIGATE_API_KEY=changeme
CMD ["omnigate2api", "-config", "config.json"]
