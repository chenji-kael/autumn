all:
	make -C cmd/extent-node/
	make -C cmd/autumn-manager/
	make -C cmd/autumn-client/
	make -C cmd/autumn-ps/
test:
	cd rangepartition/ && go test -v  -race -coverprofile=coverage.txt -covermode=atomic
