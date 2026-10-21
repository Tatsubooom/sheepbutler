# Cloud Run 用のイメージ。静的リンクした Go バイナリを distroless に載せ、非 root で動かす。
# 本体（/server）・ガード（/guard）・チャット（/chat）を同じイメージに入れ、--command で切り替える。
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN for c in server guard chat; do CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/$c ./cmd/$c || exit 1; done

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /out/guard /out/chat /
ENTRYPOINT ["/server"]
