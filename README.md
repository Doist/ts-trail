# ts-trail

Command ts-trail uploads [Tailscale SSH session recordings][1] to an S3 bucket.
It deletes uploaded files.

[1]: https://tailscale.com/kb/1011/log-mesh-traffic/#ssh-session-logs

This tool only works on Linux, and assumes that “tailscaled” process runs under systemd.
Tested with Tailscale v1.26.1.

## Setup

This program is primarily targeted for use in AWS environment, so it relies on AWS SDK [credentials discovery logic][2].
It only needs `s3:PutObject` permission to upload recordings.

[2]: https://docs.aws.amazon.com/sdkref/latest/guide/creds-config-files.html

To build from the source, you need to have [Go installed](https://go.dev/doc/install).

Then you can either:

1. Clone this repository, `cd` into the clone directory and run

       GOOS=linux GOARCH=amd64 go build

   Adjust `GOARCH` for your target architecture.
   The resulting `ts-trail` binary will be in your current directory.
2. Install it like by running

       GOOS=linux GOARCH=amd64 go install github.com/Doist/ts-trail@latest

   Adjust `GOARCH` for your target architecture.
   The resulting binary can be found under your `$GOBIN` directory if it's configured,
   or under the `$HOME/go/bin` otherwise.
   Notice that if the target OS/architecture differs from the current,
   the binary will be stored under a subdirectory named like `linux_amd64`,
   so the full path will be something like `$HOME/go/bin/linux_amd64/ts-trail`.

Copy the binary to the target host, and run it there in self-install mode:

    AWS_REGION=us-east-1 ./ts-trail -bucket=myBucketName -install

Adjust the S3 bucket name and AWS region.

The `-install` flag makes it copy itself to the canonical location, and configures/launches systemd service.
