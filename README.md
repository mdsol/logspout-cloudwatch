logspout-cloudwatch
================
A logspout adapter that pushes logs to the AWS Cloudwatch Logs service.

----------------
Overview
----------------
This software is a plugin for [logspout][1], which is a container-based application that collects Docker logs from the *other* containers run on a given Docker host. This plugin then sends those log messages on to Amazon's [Cloudwatch Logs][2] web service.


----------------
Features
----------------

* Runs as a single Docker container with access to the docker socket file -- no setup is required within the logged containers. Auto-detects Region when run in EC2, and uses IAM Role credentials when available.

* By default, names each Cloudwatch Log Stream after its container name, and groups the streams by host name. But also...

* Provides flexible, dynamic control of stream and group names, based on [templates][3]. Can assign names based on container [labels][4] or environment variables. Defines host-wide defaults while allowing per-container overrides.

* Batches messages by stream, but periodically flushes all batches to AWS, based on a configurable timeout.


----------------
Installation
----------------
The software runs in a container, so just run `docker pull mdsol/logspout`.


----------------
Workstation Usage / Outside EC2
----------------

First, make sure you're not running any containers that might be logging sensitive information -- that is, logs that you *don't* want showing up in your [Cloudwatch console][5].

1. To test the plugin, run a container that just prints out the date every few seconds:

        docker run --name=echo3 -d ubuntu bash -c \
          'while true; do echo "Hi, the date is $(date)" ; sleep 3 ; done'

    Notice that the container is run _without_ the `-t` option. Logspout will not process output from containers with a TTY attached.

2. Now run the logspout container with a route URL of `cloudwatch://us-east-1?DEBUG=1` (substitute your desired AWS region). The plugin needs AWS credentials to push data to the service, so if your credentials are set up in the [usual way][6] (at `~/.aws/credentials`), you can run:

        docker run -h $(hostname) -v ~/.aws/credentials:/root/.aws/credentials \
          --volume=/var/run/docker.sock:/tmp/docker.sock --name=logspout \
          --rm -it mdsol/logspout 'cloudwatch://us-east-1?DEBUG=1&NOEC2'


    Notice the `-h $(hostname -f)` parameter; you probably want the logging container name to share hostnames with the Docker host, because the default behavior is to group logs by hostname. The `DEBUG=1` route option allows you to make sure each batch of logs gets submitted to AWS without errors. The `NOEC2` option tells the plugin not to look for the EC2 Metadata service.

3. Navigate to the [Cloudwatch console][5], click on `Logs`, then look for a Log Group named after your workstation's hostname. You should see a Log Stream within it named `echo3`, which should be receiving your container's output messages every four seconds.


----------------
Production Usage / Inside EC2
----------------

1. Logspout needs the following policy permissions to create and write log streams and groups. Make sure your EC2 instance has a Role that includes the following:

        "Statement": [{
          "Action": [
            "logs:CreateLogGroup",
            "logs:CreateLogStream",
            "logs:DescribeLogGroups",
            "logs:DescribeLogStreams",
            "logs:PutLogEvents",
            "logs:PutRetentionPolicy"
          ],
          "Effect": "Allow",
          "Resource": "*"
        }]

2. Now run the logspout container with a route URL of `cloudwatch://auto`. The AWS Region and the IAM Role credentials will be read from the EC2 Metadata Service.

        docker run -h $(hostname) -dt --name=logspout \
          --volume=/var/run/docker.sock:/tmp/docker.sock \
          mdsol/logspout 'cloudwatch://auto'

    The `-d` and `-t` flags are optional, depending on whether you want to background the process, or run it under some supervisory daemon. But if you *do* omit the `-t` flag, you can use the environment variable `LOGSPOUT=ignore` to prevent Logspout from attempting to post its own output to AWS.


----------------
Customizing the Group and Stream Names
----------------

The first time a message is received from a given container, its Log Group and Log Stream names are computed. When planning how to group your logs, make sure the combination of these two will be unique, because if more than one container tries to write to a given stream simultaneously, errors will occur.

By default, each Log Stream is named after its associated container, and each stream's Log Group is the hostname of the container running Logspout. These two values can be overridden by setting the Environment variables `LOGSPOUT_GROUP` and `LOGSPOUT_STREAM` on the Logspout container, or on any individual log-producing container (container-specific values take precendence). In this way, precomputed values can be set for each container.

Furthermore, when the Log Group name, Log Stream name and log retention are computed, these Environment-based values are passed through Go's standard [template engine][3], and provided with the following render context:


    type RenderContext struct {
      Host       string            // container host name
      Env        map[string]string // container ENV
      Labels     map[string]string // container Labels
      Name       string            // container Name
      ID         string            // container ID
      LoggerHost string            // hostname of logging container (os.Hostname)
      InstanceID string            // EC2 Instance ID
      Region     string            // EC2 region
    }

So you may use the `{{}}` template-syntax to build complex Log Group and Log Stream names from container Labels, or from other Env vars. Here are some examples:

    # Prefix the default stream name with the EC2 Instance ID:
    LOGSPOUT_STREAM={{.InstanceID}}-{{.Name}}

    # Group streams by application and workflow stage (dev, prod, etc.),
    # where these values are set as container environment vars:
    LOGSPOUT_GROUP={{.Env.APP_NAME}}-{{.Env.STAGE_NAME}}

    # Or use container Labels to do the same thing:
    LOGSPOUT_GROUP={{.Labels.APP_NAME}}-{{.Labels.STAGE_NAME}}

    # If the labels contain the period (.) character, you can do this:
    LOGSPOUT_GROUP={{.Lbl "com.mycompany.loggroup"}}
    LOGSPOUT_STREAM={{.Lbl "com.mycompany.logstream"}}

    # Set the logs to only be retained for a period of time (defaults to retaining forever):
    # Valid values are: 1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1827, and 3653.
    # The retention policy will only be set when a log group is created, if a log group already exists its retention
    # policy will not be updated.
    LOGSPOUT_CLOUDWATCH_RETENTION_DAYS={{.Labels.LOG_RETENTION_DAYS}}

Complex settings like this are most easily applied to containers by putting them into a separate "environment file", and passing its path to docker at runtime: `docker run --env-file /path/to/file [...]`


----------------
Further Configuration
----------------

* Adding the route option `NOEC2`, as in `cloudwatch://[region]?NOEC2` causes the adapter to skip its usual check for the EC2 Metadata service, for faster startup time when running outside EC2.

* Adding the route option `DELAY=8`, as in `cloudwatch://[region]?DELAY=8` causes the adapter to push all logs to AWS every 8 seconds instead of the default of 4 seconds. If you run this adapter at scale, you may need to tune this value to avoid overloading your request rate limit on the Cloudwatch Logs API.


----------------
Contribution / Development
----------------
This software was created by Benton Roberts _(broberts@mdsol.com)_

By default, the Docker image builds from the Go source on GitHub, not from local disk, as per the instructions for [Logspout custom builds][7].



[1]: https://github.com/gliderlabs/logspout
[2]: https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/Welcome.html
[3]: https://golang.org/pkg/text/template/
[4]: https://docs.docker.com/engine/userguide/labels-custom-metadata/
[5]: https://console.aws.amazon.com/cloudwatch/home?#logs
[6]: https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-getting-started.html
[7]: https://github.com/gliderlabs/logspout/tree/master/custom
