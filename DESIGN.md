# Design

## Keeping the mapping data up to date

> Documenting a plan for keeping the mapping data up to date. Extra bonus points for implementing the solution.

I would first take a look at the application and purpose of this service: This service is critical to ensure that legitimate users can log in and do work - any downtime could result in denial of service. So while an update could be provided with a strategy like "replace the old mapping data file with a new one and restart a daemon", that would cause (at least) temporary downtime, and possibly a prolonged downtime if there is an unforeseen problem with the existing code and existing production request stream, with the new mapping data. After all, unit tests, integration tests, and QA tests only go so far.

Instead I would take a more measured "Cloud Native" approach: Use progressive delivery tools (e.g. ingress controller, Google Cloud Load Balancer, Istio, etc.) to perform "canary" rollouts of the new version of my service. This approach would allow me to still serve 100% of traffic with no downtime.

The system would first use a service's health check endpoints to ensure that instance startup worked as expected - This is just "kicking-the-tires". The system would then slowly steer a portion of the incoming request stream from one version of a service to another version of the service.

As an Engineer, I would monitor success rates and error rates of the new service (using Prometheus, Datadog, Pager events, other alerting tools), ensuring new behavior is as expected. If it is not, the change may have caused an unforeseen production problem, so I would immediately rollback and take a look at logs to understand if the behavior difference is unexpected and respond appropriately from that point on.

This process makes managing releases easier: by combining the code + the data, especially static read-only data that is of manageable size (<5MB), we can use tried-and-true tools to manage database updates with software release tools, simplifying the implementation, and reducing complexity for operations and release engineering teams. IMO the last thing an engineer in a production system wants is a behavior change that is invisible to release management tools.

This process is not one that entirely fits into code - at least, not at the level that this meat of this code challenge is asking for. At the very least, a manual checklist strategy can be used to manage service upgrades like this: Step-by-step list of actions and observations that a human must follow. As a next step, we can use agent-assisted workflows to ensure we don't accidentally miss steps. For example, Claude has an "AskUserQuestion" tool, and workflows can be designed to use that tool to progressively walk through service deployment flows to keep humans in the loop and on track (e.g. avoiding unintended reordering or skipping of steps). A more terminal example would be a completely automated workflow that handles the deploy/monitor/rollback/roll-forward decision process - K8s basically does this for underlying infrastructure, and the same strategy can be used for application specific behavior.
