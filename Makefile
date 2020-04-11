.PHONY: run deploy undeploy

run:
	go run main.go

deploy:
	GOOS=linux go build -o bin/main main.go
	sls deploy

undeploy:
	sls destroy
