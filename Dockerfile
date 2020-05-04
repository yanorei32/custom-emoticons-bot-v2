FROM golang AS build-env

MAINTAINER yanorei32

RUN go get -v github.com/zpnk/go-bitly && \
	go get -v github.com/bwmarrin/discordgo && \
	go get -v gopkg.in/yaml.v3 && \
	go get -u gopkg.in/go-playground/validator.v9 && \
	go get -u github.com/mattn/go-colorable && \
	go get -u github.com/Sirupsen/logrus

COPY ./src /work/

RUN CGO_ENABLED=0 \
	GOOS=linux \
	GOARCH=amd64 \
	go build \
		-ldflags "-s -w" \
		-o /work/app \
		/work/main.go

FROM alpine
RUN apk --no-cache add ca-certificates
COPY --from=build-env /work/app /root/app

ENTRYPOINT ["/root/app"]


