FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mockmcp ./

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/mockmcp /mockmcp
EXPOSE 9000
USER nonroot:nonroot
ENTRYPOINT ["/mockmcp"]
