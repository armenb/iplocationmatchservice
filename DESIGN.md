# Design

## Keeping the mapping data up to date

> Documenting a plan for keeping the mapping data up to date. Extra bonus points for implementing the solution.

I would first take a look at the application and purpose of this service: This service is critical to ensure that legitimate users can log in and do work - any downtime could result in denial of service. So while an update could be provided with a strategy like "replace the old mapping data file with a new one and restart a daemon", that would cause (at least) temporary downtime, and possibly a prolonged downtime if there is an unforeseen problem with the existing code and existing production request stream, with the new mapping data. After all, unit tests, integration tests, and QA tests only go so far.

Instead I would take a more measured "Cloud Native" approach: Use progressive delivery tools (e.g. ingress controller, Google Cloud Load Balancer, Istio, etc.) to perform "canary" rollouts of the new version of my service. This approach would allow me to still serve 100% of traffic with no downtime.

The system would first use a service's health check endpoints to ensure that instance startup worked as expected - This is just "kicking-the-tires". The system would then slowly steer a portion of the incoming request stream from one version of a service to another version of the service.

As an Engineer, I would monitor success rates and error rates of the new service (using Prometheus, Datadog, Pager events, other alerting tools), ensuring new behavior is as expected. If it is not, the change may have caused an unforeseen production problem, so I would immediately rollback and take a look at logs to understand if the behavior difference is unexpected and respond appropriately from that point on.

This process makes managing releases easier: by combining the code + the data, especially static read-only data that is of manageable size (<5MB), we can use tried-and-true tools to manage database updates with software release tools, simplifying the implementation, and reducing complexity for operations and release engineering teams. IMO the last thing an engineer in a production system wants is a behavior change that is invisible to release management tools.

This process is not one that entirely fits into code - at least, not at the level that this meat of this code challenge is asking for. At the very least, a manual checklist strategy can be used to manage service upgrades like this: Step-by-step list of actions and observations that a human must follow. As a next step, we can use agent-assisted workflows to ensure we don't accidentally miss steps. For example, Claude has an "AskUserQuestion" tool, and workflows can be designed to use that tool to progressively walk through service deployment flows to keep humans in the loop and on track (e.g. avoiding unintended reordering or skipping of steps). A more terminal example would be a completely automated workflow that handles the deploy/monitor/rollback/roll-forward decision process - K8s basically does this for underlying infrastructure, and the same strategy can be used for application specific behavior.

## Why refreshing the mapping data is not automated

The service ships the database inside the image, and `make fetch-db` is a deliberate, manual step. Automating the refresh (a scheduled job that downloads new data and rebuilds or redeploys the image) was considered and rejected for this scope.

IP-to-country data is the input to a login decision: a changed mapping can turn a previously allowed user into a denied one, or the reverse. That is a behavior change in production, not routine housekeeping, so it should not be applied blindly on a timer. A new database is instead treated like a new version of the code: a person fetches it, builds a new image, and rolls it out with the canary process above while watching error rates and the new `db_version` tag (see Observability) against the old one. Automation could later drive that whole flow, but the human decision to promote new data belongs in it.

## License obligation: use updated data within 30 days

MaxMind's GeoLite End User License Agreement (read 2026-11-11, page dated 2026-02-12) says that whenever MaxMind releases an updated database the licensee must "promptly use the updated version", and must "cease use of and destroy" old versions "within thirty (30) days following the release" (section 6.3). Complying with that is an operating constraint on this service, and it is the author's reading of the license, not legal advice.

It does not change the decision to deliver the code and the data together in a single image that is rolled out progressively with the canary process above. It only determines how often that process has to run: every MaxMind release starts a new image, and the rollout needs to finish well inside the 30 days, leaving time for the canary to be watched.

Two consequences follow. An image that contains superseded data can serve as a rollback target only until that deadline, and after it old images need to be removed from the registry and from any node caches.

The service cannot enforce a calendar deadline, so this is a job for operations tooling. An ops-specific AI agent (or a plain scheduled job) can watch for new MaxMind releases, compare the latest release with the `db_version` that the running pods report (it is on every log line and metric tag, see Observability), and remind a human or open tickets (JIRA or similar) as the deadline approaches. The decision to promote the new data stays with a person, consistent with "Why refreshing the mapping data is not automated".

This describes the free GeoLite license. A commercial license (for example GeoIP2) has its own terms, and whether it carries a similar constraint has not been checked.

## Behavior decisions

**Unknown locations fail closed.** If the database cannot place an IP address in a country, the answer is `match: false`, exactly as for a known address in a different country. Some consumers of this interface require this level of caution, so the service is cautious from the outset rather than leaving it to each caller. "Unknown" covers addresses absent from the database (private and reserved ranges), and networks whose record has no country, only a registered country. In the 2026-11-10 GeoLite2-Country build that is 1,500 of 1,060,889 networks, for example the anycast range `1.1.1.0/24`. The API returns only the boolean; the service records the distinction (`result: unknown_ip`) in its logs and metrics so operators can see how often it happens. A consumer that needs to choose fail-open for unknown locations would need the API to return the resolved country; that is not built. Internally the question is answered in three explicit steps, validate the allow-list, resolve the country (`Checker.Country`, empty if unknown) and decide (`InCountries`), so a future `denied_countries` variant can reuse the same lookup and only change the decision.

**An empty `allowed_countries` is a 400.** A caller whose customer has no country restriction can see that for itself and skip the call, so an empty list is treated as a caller mistake rather than guessed at as either "allow everyone" or "allow no one".

**The allow-list has a length limit, 50 by default.** `allowed_countries` accepts at most 50 entries (the `-max-allowed-countries` setting), enough for every country in Europe plus a handful of others. A longer list is rejected with a 400 instead of being served. If a caller hits the limit, that is a prompt to work with the customer: do they really want this specific limit raised, or do they want to say "everywhere except these few countries" (a possible `denied_countries` field, mutually exclusive with `allowed_countries`)? The product can iterate from there. The limit is about the shape of the product, not performance: validating and matching even 200,000 entries was measured at roughly a quarter of a millisecond, and the 1 MiB body cap already bounds the worst case. The length is checked before the entries, and the error says how many were sent and what the limit is.

**Inputs are strict.** The `ip` must be a bare IPv4 or IPv6 address (no port, no brackets, no `X-Forwarded-For` list), the IP may have no surrounding whitespace, country codes must be two letters, the body must be one JSON object with exactly the documented keys (names are case-sensitive and may not repeat, and unknown keys are rejected), and bodies are capped at 1 MiB. A caller using the endpoint incorrectly learns immediately from a 400 instead of being silently interpreted. Accepting `host:port` and bracketed IPv6 would be a small addition (`netip.ParseAddrPort`) if gateways turn out to send them. The contract is published as an OpenAPI schema in `api/openapi.yaml` so callers can validate against it. Go's default JSON decoder would have accepted keys in the wrong case and let a repeated key silently win, which is the kind of disagreement between a login control and its caller that should not exist, so the keys are checked explicitly. The body is parsed as JSON whatever the `Content-Type`, so a plain `curl -d` works.

## Observability

Every request except the health probe produces one compact JSON log line (transport, status or gRPC code, result, resolved country, IP, database version, duration), and, when `-statsd-addr` is set, a counter and a timer sent as DogStatsD with a `db_version` tag. Both carry the database version because the canary process above depends on comparing the new pods' success and error rates to the old pods'.

The caller (the API gateway) holds richer context about the requesting user and is the natural place for end-to-end correlation, but a rollout of this service needs its own signal, so a minimal, dependency-free client is built in. It is off by default, queues metrics off the request path, and drops rather than blocks. No metrics framework was specified for this exercise; adapting it to another one means implementing the two-method `telemetry.Sink` interface.

Logs and metrics are billed by volume, and this service may be called once per HTTP resource a user loads rather than once per login. The log line therefore has a short, fixed set of fields, omits the country list and request body, truncates the caller-supplied IP, and excludes the health probe, which Kubernetes calls every few seconds on every pod. The IP address is personal data in some jurisdictions; removing it is a one-field change.

## Gateway failure policy and introduction (recommended; to be confirmed)

The service answers one question. What the gateway does with the answer, and what it does when there is no answer, is a separate decision, and for a login dependency the more consequential one. The service cannot enforce it, so this is a recommendation to agree with the product owner and the gateway team; apart from the shadow-mode introduction, which the owner has endorsed, it has not been decided.

- **Introduce it in shadow mode.** The gateway calls the service for restricted customers but ignores the answer for a while. The team learns how the service behaves and performs from dashboards and alert rules built on its logs and metrics, and the gateway starts acting on the answer only after a reasonable history of acceptable behavior. Shadow mode also checks the gateway's side of the contract at no cost to any user: a gateway that forwards the wrong address (a load balancer's, an internal one) shows up as a wall of `unknown_ip` before anyone is denied.
- **Scope.** Only customers that have turned on a country restriction call the service. Everyone else has no dependency on it (an empty allow-list is a 400 by design, so the gateway skips the call).
- **When enforcing, and no answer arrives** (unreachable, timeout, 5xx, or gRPC `Unavailable` or `Internal`): fail closed, meaning the restricted customer's user is denied. That matches unknown locations failing closed: a customer who asked for the restriction asked for it to hold. The user should see a distinct, retryable error, not a "wrong country" message, so support can tell an outage from a policy denial.
- **Bounded waiting.** A short call timeout taken from the gateway's login latency budget (handler times in the live samples so far are tens to hundreds of microseconds; the network and queuing dominate), at most one retry for a connection-level failure (the call is idempotent), and a circuit breaker so that a dead service costs a login a fast failure instead of the full timeout.
- **Break-glass (optional).** A gateway-side, per-customer switch to fail open, needing an explicit decision by a named person, audited, and expiring on its own, is a good tool. If the shadow period builds a solid history it may be unnecessary, so decide once that history exists. Failing open for a customer is a product decision; the default stays closed.
- **A 400 is not an outage.** It means the gateway sent a bad request (for example `ip:port`, or a field name in the wrong case). It is a gateway bug to fix, not to retry.
- **Targets.** No availability or latency targets are defined. They should be set with the gateway team from the shadow period and production-like load tests; so far the evidence is micro-benchmarks only.

## Canary gates and signals

A code release is judged by errors and latency. A data release changes decisions, not errors, so it is judged by decisions. A changed country assignment changes a decision only for a request whose allowed list contains one of the two countries but not the other; a change that touches no list changes no answer. Those that do change answers show up as a shift in the mix of results.

**What is measured now.** The counter `iplocationmatch.requests` carries `transport`, `result` (`match`, `no_match`, `unknown_ip`, `invalid` or `error`) and `db_version`; the timer `iplocationmatch.request.duration` carries `transport` and `db_version`; and every request log line has the status or code, result, resolved country, IP, `db_version` and duration.

**Canary gates** (compare canary pods with baseline pods over the same period, since traffic is split between them):
- *Code release:* the error share (`result:error`, 5xx, `Internal`, `Unavailable`) and latency stay inside the baseline's range.
- *Data release:* the share of each `result`, by `db_version`, stays within a threshold agreed in advance. A MaxMind update legitimately moves some networks between countries, so the expectation is "a small change", not "no change"; set the threshold from earlier releases. A jump in `unknown_ip` or `no_match` goes to a human instead of being promoted automatically.
- *Replay, optional and not built:* before the canary, resolve a day of logged requests with the new database offline, compare the result with the old one, and investigate the differences (which countries moved, which networks). The log records each request's IP and resolved country but not its allowed list, which is left out to keep the log compact, so such a replay compares countries, not decisions. The share of changed countries is an upper bound on the share of changed decisions.

**Signals for downstream tooling.** Dashboards and alert rules are built downstream of log and metric processing and are out of scope for this service; its job is to emit signals that make them possible. Rules can key on:
- the `unknown_ip` share: the highest-impact failure of the design is a gateway that forwards the wrong address, which denies every restricted login and looks exactly like a wall of `unknown_ip`;
- the `invalid` share rising, which means the gateway's requests changed shape;
- the error share, latency, and the request rate dropping to zero;
- the age of `db_version` on running pods, for the 30-day license clock (see "License obligation");
- silence in the metrics, since the metrics client drops rather than blocks (`Dropped()` counts the drops but is not exported as a metric).

## Security model: no authentication or TLS

The service speaks plain HTTP and unencrypted gRPC and checks no credentials. This is intentional for the intended deployment: it is an internal service called by the API gateway inside the cluster. It handles no secrets (a request is an IP address and country codes; the response is one boolean), and authentication, authorization and transport encryption between services are better provided once by the platform than reimplemented in each service.

What that assumes: only the gateway can reach the service port. Anyone who can reach it can query the geolocation of arbitrary IP addresses and generate load. If the cluster network is not trusted, add a `NetworkPolicy` that admits only the gateway, or mesh mTLS (for example Istio, which the rollout section already mentions), or TLS and a token in the service itself. None of these is included in `deploy/k8s/`.

## Extensions beyond the request, and why

- **Concurrency and speed.** Many requests must be handled at once, and the service may be called for every HTTP resource a user requests, not just at the start of a login session, so lookups have to be fast. The database is memory-mapped read-only and shared by all requests without locks, and a race-detector test and an integration test drive concurrent mixed HTTP and gRPC load.
- **Strict, bounded JSON with a published schema.** To promote interoperability: callers can validate against `api/openapi.yaml`, and the server enforces it instead of tolerating drift.
- **Health endpoint.** A practical requirement of Kubernetes and other cloud-native harnesses, and the check the rollout tooling uses to "kick the tires" on a new instance before shifting traffic.
- **Graceful shutdown.** On SIGTERM the service stops accepting work and drains in-flight requests (10 seconds); a second signal terminates immediately. Zero-downtime rollouts and canary shifts depend on pods finishing their requests as they are replaced.
- **Request logging and metrics with the database version.** See Observability: it is what makes the canary process above measurable.
- **IPv4-mapped IPv6 normalization.** Load balancers and ingress controllers commonly present IPv4 clients as `::ffff:a.b.c.d`; these are looked up as IPv4 so they resolve correctly.
- **Tests that need no licensed data.** Unit tests build a small GeoLite2-shaped database on the fly, an integration test runs the real binary against it, and an optional mode runs the same suite against the real database.
