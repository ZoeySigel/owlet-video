FROM golang:1.24-alpine AS build
ENV GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /owlet-video .

FROM alpine:3.21
RUN addgroup -S owlet && adduser -S -G owlet owlet
COPY --from=build /owlet-video /usr/local/bin/owlet-video
USER owlet
ENTRYPOINT ["/usr/local/bin/owlet-video"]
