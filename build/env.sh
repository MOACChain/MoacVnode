#!/bin/sh
#modified to fit GOOGLE CLOUD
set -e

if [ ! -f "build/env.sh" ]; then
    echo "$0 must be run from the root of the repository."
    exit 2
fi

# Create fake Go workspace under build if it doesn't exist yet.
workspace="$PWD/build/_workspace"
# Record the root of the repository
root="$PWD"
vnodeDir="$workspace/src/github.com/MOACChain"
if [ ! -L "$vnodeDir/MoacVnode" ]; then
    mkdir -p "$vnodeDir"
    cd "$vnodeDir"
    echo "Make" $vnodeDir
    ln -s ../../../../../. MoacVnode
    #Add a library path
    ln -s ../../../../../../MoacLib MoacLib
    cd "$root"
fi

# Set up the environment to use the workspace.
GOPATH="$workspace"
export GOPATH
echo Set GOPATH $GOPATH
# Enable the GO module, still has issue with go.mod, used the old vendor
# GO111MODULE=on
GOSUMDB=off

# To used with go.mod with private repos, must setup these info
# to work, please see the following article
# https://medium.com/mabar/today-i-learned-fix-go-get-private-repository-return-error-reading-sum-golang-org-lookup-93058a058dd8
export GOPRIVATE="github.com/MOACChain/MoacVnode, github.com/MOACChain/MoacLib"
# Run the command inside the workspace.
cd "$vnodeDir/MoacVnode"
PWD="$vnodeDir/MoacVnode"
# Launch the arguments with the configured environment.
exec "$@"
