## Installation


### Mac Installation
To install in mac os, install the following packages.

    $go get golang.org/x/net/context
    $go get google.golang.org/grpc
    $go get -u github.com/golang/protobuf/protoc-gen-go

    $rm '/usr/local/bin/protoc'
    $brew install protobuf

    $sudo chown -R `whoami`:admin /usr/local/bin
    $sudo chown -R `whoami`:admin /usr/local/include
    $sudo chown -R `whoami`:admin /usr/local/lib

    $brew link --overwrite protobuf

in ~/.bashrc add protoc-gen-go folder to your $PATH

    export PATH=$PATH:/Users/[YourNameHere]/go/bin

To generate go file for proto. Also, the proto file for vnode and scs should be the same

    ./scs/proto$protoc --go_out=plugins=grpc:. *.proto
    ./proto$protoc --go_out=plugins=grpc:. *.proto
    
    
### Compilation in commandline
To compile the moac project under commandline condition:

    ~$cd go/src/github.com/MOACChain/MoacVnode
    ~/go/src/github.com/MOACChain/MoacVnode$install_pkg.sh
    ~/go/src/github.com/MOACChain/MoacVnode$make vnode
    >>> /usr/local/Cellar/go/1.9.1/libexec/bin/go install -ldflags -X main.gitCommit=40038e6616d8eb692cb965ff78119d5caac35f28 -s -v ./cmd/moac
    github.com/MOACChain/MoacVnode/cmd/moac
    ~/go/src/github.com/MOACChain/MoacVnode$./build/bin/moac -your-parameters

### To start vnode server


moac --testnet --rpccorsdomain "*" --rpc --rpcport "8545" --rpcapi "chain3,mc,net"


### Rebuild bindata.go
The binary data inside ./internal/jsre/deps/bindata.go will need to be regenerate once chain3.js or bignumber.js files are updated.

1. Git clone https://github.com/jteeuwen/go-bindata/ into your local machine
        
        $git clone https://github.com/jteeuwen/go-bindata.git
    
2. Under ./go-bindata folder, build your go-bindata executable file

        $cd go-bindata
        $cd go-bindata
        $go build
    
3. Copy the executable go-bindata file to your moac-vnode/internal/jsre/deps/ folder

4. Regenerate bindata.go file

        ./moac-vnode/internal/jsre/deps$./go-bindata -nometadata -pkg deps -o bindata.go bignumber.js chain3.js
 
