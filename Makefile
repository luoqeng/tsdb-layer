gen-proto: install-go-proto-plugin
	protoc  --proto_path=./protos --go_out=plugins=grpc:./protos/.gen/ ./protos/rpc.proto
	protoc  --proto_path=./protos --grpc-gateway_out=logtostderr=true:./protos/.gen/ ./protos/rpc.proto

install-go-proto-plugin:
	go get -u github.com/golang/protobuf/protoc-gen-go
	go get -u github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway
	go get -u github.com/grpc-ecosystem/grpc-gateway/protoc-gen-swagger

bench:
	go run ./src/cmd/bench/main.go -numSeries 1000000 -batchSize 256 -numWorkers 300 -duration 5s