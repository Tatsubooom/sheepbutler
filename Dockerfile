# Cloud Run 用のイメージ。静的リンクした Go バイナリを distroless に載せ、非 root で動かす。
# 本体（/server）とガード（/guard）を同じイメージに入れ、ガードは --command /guard で起動する。
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/guard ./cmd/guard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /out/guard /
ENTRYPOINT ["/server"]
