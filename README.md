# Kamal Proxy - A minimal HTTP/UDP proxy for zero-downtime deployments


## What it does

Kamal Proxy is a lightweight proxy server, designed to make it easy to coordinate
zero-downtime deployments. By running your web applications and UDP services behind 
Kamal Proxy, you can deploy changes to them without interrupting any of the traffic 
that's in progress. No particular cooperation from an application is required for 
this to work.

Kamal Proxy supports both **HTTP/HTTPS** and **UDP/WebRTC** traffic, providing 
session-aware routing and graceful deployment strategies for real-time applications.

Kamal Proxy is designed to work as part of [Kamal](https://kamal-deploy.org/),
which provides a complete deployment experience including container packaging
and provisioning. However, Kamal Proxy could also be used standalone or as part
of other deployment tooling.


## A quick overview

To run an instance of the proxy, use the `kamal-proxy run` command. There's no
configuration file, but there are some options you can specify if the defaults
aren't right for your application.

For example, to run the proxy with custom HTTP and UDP ports:

    kamal-proxy run --http-port 8080 --udp-port 9090

Run `kamal-proxy help run` to see the full list of options.

To route traffic through the proxy to a web application, you `deploy` instances
of the application to the proxy. Deploying an instance makes it available to the
proxy, and replaces the instance it was using before (if any).

Use the format `hostname:port` when specifying the instance to deploy.

### HTTP Service Deployment

For HTTP services:

    kamal-proxy deploy service1 --target web-1:3000

This will instruct the proxy to register `web-1:3000` to receive HTTP traffic under
the service name `service1`. It will immediately begin running HTTP health
checks to ensure it's reachable and working and, as soon as those health checks
succeed, will start routing traffic to it.

### UDP Service Deployment

For UDP services:

    kamal-proxy deploy-udp gameserver --target game:9999 --port 9999

This deploys a UDP service on port 9999, routing packets to the target server.

### WebRTC Service Deployment

For WebRTC services with dynamic port allocation:

    kamal-proxy deploy-udp livekit --target livekit:7880 --protocol webrtc --port-ranges 10000-20000,20001-30000

This deploys a WebRTC service with RTP ports allocated from the first range and RTCP ports from the second range.

If the instance fails to become healthy within a reasonable time, the `deploy`
command will stop the deployment and return a non-zero exit code, allowing
deployment scripts to handle the failure appropriately.

Each deployment takes over all the traffic from the previously deployed
instance. As soon as Kamal Proxy determines that the new instance is healthy,
it will route all new traffic to that instance.

The `deploy` command also waits for traffic to drain from the old instance before
returning. This means it's safe to remove the old instance as soon as `deploy`
returns successfully, without interrupting any in-flight requests.

Because traffic is only routed to a new instance once it's healthy, and traffic
is drained completely from old instances before they are removed, deployments
take place with zero downtime.

### Zero-Downtime UDP Deployments

UDP services support zero-downtime deployments with session preservation:

    kamal-proxy deploy-udp gameserver --target game:9999 --port 9999 --zero-downtime --drain-strategy graceful --drain-timeout 60s

This performs a graceful deployment where:
- New targets are health-checked before receiving traffic
- Existing sessions continue on old targets during the drain period
- Sessions are gracefully terminated after the drain timeout
- WebRTC services receive RTCP BYE messages for clean disconnection

Available drain strategies:
- `graceful`: Allow existing sessions to complete naturally within the timeout
- `immediate`: Terminate all sessions immediately after traffic switch
- `timeout`: Force terminate sessions after a maximum wait time

### Customizing the health check

By default, Kamal Proxy will test the health of each service by sending a `GET`
request to `/up`, once per second. A `200` response is considered healthy.

If you need to customize the health checks for your application, there are a
few `deploy` flags you can use. See the help for `--health-check-path`,
`--health-check-timeout`, and `--health-check-interval`.

For example, to change the health check path to something other than `/up`, you
could:

    kamal-proxy deploy service1 --target web-1:3000 --health-check-path web/index.html

### Host-based routing

Host-based routing allows you to run multiple applications on the same server,
using a single instance of Kamal Proxy to route traffic to all of them.

When deploying an instance, you can specify a host that it should serve traffic
for:

    kamal-proxy deploy service1 --target web-1:3000 --host app1.example.com

When deployed in this way, the instance will only receive traffic for the
specified host. By deploying multiple instances, each with their own host, you
can run multiple applications on the same server without port conflicts.

Only one service at a time can route a specific host:

    kamal-proxy deploy service1 --target web-1:3000 --host app1.example.com
    kamal-proxy deploy service2 --target web-2:3000 --host app1.example.com # returns "Error: host is used by another service"
    kamal-proxy remove service1
    kamal-proxy deploy service2 --target web-2:3000 --host app1.example.com # succeeds


### Path-based routing

For applications that split their traffic to different services based on the
request path, you can use path-based routing to mount services under different
path prefixes.

For example, to send all the requests for paths begining with `/api` to web-1,
and the rest to web-2:

    kamal-proxy deploy service1 --target web-1:3000 --path-prefix=/api
    kamal-proxy deploy service2 --target web-2:3000

By default, the path prefix will be stripped from the request before it is
forwarded upstream. So in the example above, a request to `/api/users/123` will
be forwarded to `web-1` as `/users/123`. To instead forward the request with
the original path (including the prefix), specify `--strip-path-prefix=false`:

    kamal-proxy deploy service1 --target web-1:3000 --path-prefix=/api --strip-path-prefix=false


### Automatic TLS

Kamal Proxy can automatically obtain and renew TLS certificates for your
applications. To enable this, add the `--tls` flag when deploying an instance:

    kamal-proxy deploy service1 --target web-1:3000 --host app1.example.com --tls

Automatic TLS requires that hosts are specified (to ensure that certificates
are not maliciously requests for arbitrary hostnames).

Additionally, when using path-based routing, TLS options must be set on the
root path. Services deployed to other paths on the same host will use the same
TLS settings as those specified for the root path.


### Custom TLS certificate

When you obtained your TLS certificate manually, manage your own certificate authority,
or need to install Cloudflare origin certificate, you can manually specify path to
your certificate file and the corresponding private key:

    kamal-proxy deploy service1 --target web-1:3000 --host app1.example.com --tls --tls-certificate-path cert.pem --tls-private-key-path key.pem


## UDP and WebRTC Features

### Session Management

UDP services support session affinity to ensure packets from the same client consistently reach the same backend target:

    kamal-proxy deploy-udp service --target backend:9999 --port 9999 --session-affinity --affinity-strategy 5-tuple --session-timeout 300s

Session affinity strategies:
- `5-tuple`: Route based on source IP, source port, destination IP, destination port, and protocol
- `source-ip`: Route based on client IP address only
- `source-port`: Route based on client IP and port

### WebRTC Port Management

WebRTC services support dynamic port allocation with configurable policies:

    kamal-proxy deploy-udp livekit --target livekit:7880 --protocol webrtc --port-ranges 10000-15000,20000-25000 --rtcp-policy adjacent

RTCP port policies:
- `adjacent`: RTCP port is RTP port + 1 (e.g., RTP: 10000, RTCP: 10001)
- `separate-range`: RTCP ports allocated from a separate range
- `auto`: Automatically choose the best policy based on port availability

### Health Checking for UDP Services

UDP services use TCP health checks on the target's management port to verify service availability before routing traffic. This ensures only healthy targets receive UDP packets during deployments.

### Common Use Cases

**Game Servers**:
```bash
kamal-proxy deploy-udp gameserver --target game1:25565 --port 25565 --session-affinity --session-timeout 1800s
```

**LiveKit WebRTC**:
```bash
kamal-proxy deploy-udp livekit --target livekit:7880 --protocol webrtc --port-ranges 10000-20000 --zero-downtime --drain-strategy graceful
```

**Media Streaming**:
```bash
kamal-proxy deploy-udp streamer --target stream:8000 --protocol webrtc --port-ranges 15000-16000,25000-26000 --rtcp-policy separate-range
```


## Specifying `run` options with environment variables

In some environments, like when running a Docker container, it can be convenient
to specify `run` options using environment variables. This avoids having to
update the `CMD` in the Dockerfile to change the options. To support this,
`kamal-proxy run` will read each of its options from environment variables if they
are set. For example, setting the HTTP port can be done with either:

    kamal-proxy run --http-port 8080

or:

    HTTP_PORT=8080 kamal-proxy run

If any of the environment variables conflict with something else in your
environment, you can prefix them with `KAMAL_PROXY_` to disambiguate them. For
example:

    KAMAL_PROXY_HTTP_PORT=8080 kamal-proxy run


## Building

To build Kamal Proxy locally, if you have a working Go environment you can:

    make

Alternatively, build as a Docker container:

    make docker


## Trying it out

See the [example](./example) folder for a Docker Compose setup that you can use
to try out the proxy commands.
