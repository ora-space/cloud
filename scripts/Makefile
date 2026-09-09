.PHONY: build run test check fmt lint docker-build
build run test check lint:
	cd .. && task $@
fmt:
	cd .. && task format
docker-build:
	cd .. && docker build -t ora-cloud:phase-one -f scripts/Dockerfile .
