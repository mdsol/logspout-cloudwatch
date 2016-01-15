logspout-cloudwatch
================
A logspout adapter that pushes logs to the AWS Cloudwatch Logs service.

----------------
Overview
----------------
This software is a plugin for [logspout][1], which is an application that reads the Docker logs from all containers not allocated with a tty (that is, *not* run with the `-t` option). This plugin sends the log messages on to Amazon's [Cloudwatch Logs][2] web service.


----------------
Features:
----------------

* Logspout is run as a Docker container alongside the logged containers. No setup is required within the logged containers.

* Cloudwatch defines unique Log Streams, and requires that each be assigned to a Log Group. By default, container logs are named after their container name, and grouped by host.

* Provides flexible, dynamic control of stream and group names, based on [templates][3]. Assign names based on a container's [labels][4] or environment variables. Set host-wide defaults with per-container overrides.

* Batches messages by stream, but periodically flushes all batches up to AWS, on a configurable timeout.


----------------
Installation
----------------
The software runs in a container, so just `docker pull mdsol/logspout`.

----------------
Usage
----------------

1. First, make sure you're not running any containers that might be logging sensitive information -- that is, logs that you *don't* want showing up in your [Cloudwatch Logs console][5]. Use `docker ps -a` to check.

2. Run a container that just spits out the date every few seconds:

    docker run -h $(hostname -f) --name=echo3 -d --entrypoint=bash ubuntu \
      -c 'while true; do echo "Hi, the date is $(date)" ; sleep 3 ; done'

3.

----------------
Usage
----------------

[1]: https://github.com/gliderlabs/logspout
[2]: https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/Welcome.html
[3]: https://golang.org/pkg/text/template/
[4]: https://docs.docker.com/engine/userguide/labels-custom-metadata/
[5]: https://console.aws.amazon.com/cloudwatch/home?#logs
