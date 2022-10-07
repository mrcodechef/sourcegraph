#!/usr/bin/env bash

# This script builds the executor image as a GCP boot disk image and as an AWS AMI.

cd "$(dirname "${BASH_SOURCE[0]}")"/../../../..
set -eu

OUTPUT=$(mktemp -d -t sgdockerbuild_XXXXXXX)
cleanup() {
  rm -rf "$OUTPUT"
}
trap cleanup EXIT

# Capture src cli version before we reconfigure the go environment.
SRC_CLI_VERSION="$(go run ./internal/cmd/src-cli-version/main.go)"

# Environment for building linux binaries
export GO111MODULE=on
export GOARCH=amd64
export GOOS=linux
export CGO_ENABLED=0
export VERSION

echo "--- go build"
pushd ./enterprise/cmd/executor 1>/dev/null
pkg="github.com/sourcegraph/sourcegraph/enterprise/cmd/executor"
bin_name="$OUTPUT/$(basename $pkg)"
go build -trimpath -ldflags "-X github.com/sourcegraph/sourcegraph/internal/version.version=$VERSION -X github.com/sourcegraph/sourcegraph/internal/version.timestamp=$(date +%s)" -buildmode exe -tags dist -o "$bin_name" "$pkg"
popd 1>/dev/null

echo "--- create binary artifacts"
# Setup new release folder that contains binary, info text.
mkdir -p "enterprise/cmd/executor/vm-image/artifacts/executor/$(git rev-parse HEAD)"
pushd "enterprise/cmd/executor/vm-image/artifacts/executor/$(git rev-parse HEAD)" 1>/dev/null

echo "executor built from https://github.com/sourcegraph/sourcegraph" >info.txt
echo >>info.txt
git log -n1 >>info.txt
mkdir -p linux-amd64
# Copy binary into new folder
cp "$bin_name" linux-amd64/executor
sha256sum linux-amd64/executor >>linux-amd64/executor_SHA256SUM
popd 1>/dev/null
# Upload the new release folder
echo "--- upload binary artifacts"
gsutil cp -r enterprise/cmd/executor/vm-image/artifacts/executor gs://sourcegraph-artifacts
gsutil iam ch allUsers:objectViewer gs://sourcegraph-artifacts

# Fetch the e2e builder service account so we can spawn a packer VM.
echo "--- gcp secret"
gcloud secrets versions access latest --secret=e2e-builder-sa-key --quiet --project=sourcegraph-ci >"$OUTPUT/builder-sa-key.json"

echo "--- packer build"
# Copy files into workspace.
cp .tool-versions "$OUTPUT"
pushd ./enterprise/cmd/executor/vm-image 1>/dev/null
cp executor.pkr.hcl "$OUTPUT"
cp install.sh "$OUTPUT"
cp aws_regions.json "$OUTPUT"
popd 1>/dev/null
pushd ./docker-images 1>/dev/null
cp -R executor-vm "$OUTPUT"

export PKR_VAR_name
PKR_VAR_name="${IMAGE_FAMILY}-${BUILDKITE_BUILD_NUMBER}"
export PKR_VAR_version="${VERSION}"
export PKR_VAR_src_cli_version=${SRC_CLI_VERSION}
export PKR_VAR_aws_access_key=${AWS_EXECUTOR_AMI_ACCESS_KEY}
export PKR_VAR_aws_secret_key=${AWS_EXECUTOR_AMI_SECRET_KEY}
# This should prevent some occurrences of Failed waiting for AMI failures:
# https://austincloud.guru/2020/05/14/long-running-packer-builds-failing/
export PKR_VAR_aws_max_attempts=480
export PKR_VAR_aws_poll_delay_seconds=5
export PKR_VAR_aws_regions
if [ "${EXECUTOR_IS_TAGGED_RELEASE}" = "true" ]; then
  PKR_VAR_aws_regions="$(jq -r '.' <aws_regions.json)"
else
  PKR_VAR_aws_regions='["us-west-2"]'
fi

pushd "$OUTPUT" 1>/dev/null
packer init executor.pkr.hcl
packer build -force executor.pkr.hcl
popd 1>/dev/null
