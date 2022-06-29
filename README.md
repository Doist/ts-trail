# ts-trail

Command ts-trail uploads [Tailscale SSH session recordings][1] to an S3 bucket.
It deletes uploaded files.

[1]: https://tailscale.com/kb/1011/log-mesh-traffic/#ssh-session-logs

This tool only works on Linux, and assumes that “tailscaled” process runs under systemd.
Tested with Tailscale v1.26.1.
