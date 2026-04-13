FROM golang:1.25.9 AS builder

WORKDIR /workspace

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY LICENSE .
COPY cmd/main.go cmd/main.go
COPY internal/controller/ internal/controller/

RUN go install github.com/google/go-licenses@latest \
    && go-licenses save ./... --save_path=/credits

FROM scratch AS export
COPY --from=builder /credits /credits
