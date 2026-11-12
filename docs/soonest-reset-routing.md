# Soonest-reset routing

This fork is based on CLIProxyAPI v8.0.67 and adds an opt-in native selector:

```yaml
routing:
  strategy: "soonest-reset"
  session-affinity: true
  session-affinity-ttl: "1h"
```

The selector spends allowance from the eligible account whose longest observed
quota window resets first. Claude uses the general seven-day window, falling
back to the representative quota reset when no general weekly reset is supplied.
Monthly overage billing boundaries are excluded. Codex uses the longest reported
primary or secondary window, including weekly-only accounts; the primary window
is not assumed to be five hours.

Existing model support, priority tiers, disabled credentials, cooldowns and Codex
WebSocket transport preferences still determine eligibility. Reset time orders
the eligible credentials within the highest available priority tier. Ties and
providers without supported reset signals use stable fill-first order.

With session affinity enabled, an established conversation keeps its credential
until it becomes unavailable or the binding expires through inactivity. New
sessions and failover assignments use the new order. The affinity TTL refreshes
with activity; it is not a periodic rebalance interval. Retained WebSocket turns
continue to use their established upstream connection.

## Prefer included Codex allowance before credits

Give the included-allowance credential a higher priority (for example `100`) than
the credits credential (`0`), then enable:

```yaml
routing:
  strategy: "soonest-reset"
  session-affinity: true
  codex-prefer-higher-priority: true
```

Codex sessions use the highest available priority tier, including sessions that
were previously bound to a fallback credential. The lower tier serves requests
when the higher tier is unavailable because of quota exhaustion, outages or other
account failures. After recovery, the next selection returns to the higher tier.
Equal-priority accounts retain affinity and use soonest-reset for new assignments.
This is an account priority policy; it does not detect billing plans or disable
credits within an upstream account. Configure priority through credential settings.

This option defaults to false and affects only Codex. It works with every routing
strategy and is ignored when affinity is disabled or `session-affinity-rebind-on`
is `never`. Retained WebSocket turns keep their established upstream connection;
priority takes effect on the next credential selection. Home dispatch is unchanged.

## Reserve allowance for personal use

Set `quota_reserve_percent` on the Business credential through
`PATCH /v8/management/credentials/fields`:

```json
{"name":"<business credential ID>","quota_reserve_percent":20}
```

The field is saved in that credential's auth file. Values from 0 to 100 are
accepted; missing, null or 0 disables the reserve. Keep the Plus/credits
credential at 0 (or leave the field absent), and retain Business priority 100
versus credits priority 0 with `codex-prefer-higher-priority: true`.

Business is eligible while more than 20% remains in every reported current base
window. At 80% used or higher in either window, it becomes unavailable to proxy
requests, including existing affinity bindings. Normal fallback selects the
credits account. When the blocking windows reset, Business becomes eligible
again. Under `rebind-on: never`, bound requests return an error at the reserve
instead of moving accounts. The setting applies to Codex credentials with every
local routing strategy; it does not alter the upstream account's limits.

This is a soft reserve based on passive quota snapshots. Usage observations are
learned again after a proxy restart. Unknown usage permits discovery; known usage
at the reserve without a readable reset stays protected until a new observation
or settings change. In-flight requests and retained upstream WebSocket turns can
overshoot the threshold. Set a larger reserve if a safety margin is needed.
Home dispatch is unchanged. The reserve does not restrict direct use in the app.

[OpenAI's pricing documentation](https://developers.openai.com/codex/pricing)
states that ChatGPT Work and Codex share usage. This reserve protects that shared
allowance; it is not a guarantee about every ChatGPT feature's separate limits.

## Observations and rollover

Quota data is collected passively from normal upstream response headers. Routing
does not poll provider quota endpoints, issue artificial model requests, redeem
reset credits, or enable paid overage. Existing upstream account billing settings
continue to apply.

An eligible Claude or Codex account with no readable future reset receives a
bounded observation opportunity: one incoming request per hour. The first request
after a previously observed window expires may probe immediately, even if an
earlier probe happened less than an hour before the reset. Other requests prefer
known future resets until the next probe is due. Session affinity can retain a
session assigned during a probe, just as it retains any other assignment.
Token-count and selection-only requests follow existing affinity bindings, or
use known ordering on a miss, without consuming probes or creating new bindings
under this strategy.

Snapshots are kept in memory and learned again after a restart. Disabled,
unsupported or cooling credentials are never probed. Concurrent selections share
probe bookkeeping, preventing a burst of refresh attempts. Stream observations
use the time headers arrived; an older stream finishing later cannot replace a
newer snapshot. Replacing an account clears the former account's observation.
Partial responses that would discard a still-future reset of the scored window
leave its snapshot and receipt time intact. Once that reset expires, an
observation of a different window shape can replace it.

## Build and rollback

The Linux hub uses an amd64 binary built with:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o cli-proxy-api ./cmd/server
```

Use the v8 configuration API or edit `config.yaml` to select the strategy. The
deprecated v0 routing-strategy setter does not recognise the new value.

To revert routing while keeping this binary, change the strategy to `round-robin`.
Before replacing a production binary, retain the original binary and a private
configuration backup. Restore both to roll back the deployment. Standard upstream
installers and updates replace the custom binary; rebuild this fork when adopting
upstream updates.

## Attribution

The native selector and observation handling are adapted from
[steelzeh's upstream PR #7246](https://github.com/router-for-me/CLIProxyAPI/pull/7246),
head `972b70aae048363775bd2b31f8c4ad442123cd27`. This fork additionally refreshes
expired observations and covers rollover, concurrent probes and session-affinity
failover with regression tests.
