# AWS alarms: what reaches KARMAX

The `aws-alarms` loop reads every usable profile's CloudWatch alarms every 5 minutes
(`internal/sources/aws`). Each alarm it tells becomes an event, and each event starts a
model turn. An alarm that flaps OK → ALARM → OK would otherwise cost a turn on every flip,
so a gate (`internal/sources/aws/gate.go`) sits between what a poll finds and what it
publishes. It is set under `sources.aws.alarms` in `karmax.yaml`; see the commented block
in `karmax.yaml.example`.

| Key | Default | What it does |
|---|---|---|
| `debounce_polls` | **3 (on)** | An alarm is told only once it has been in ALARM at this many polls in a row (3 = 15 minutes). Its OK is told only if its ALARM was. An ALARM that ends sooner tells nothing and is counted as a flip. `1` (or `0`) tells every ALARM at once, which was the behaviour before the gate. |
| `cooldown` | off | After an alarm's ALARM is told, its next ALARMs are held for this long (6h is a sensible value). An ALARM that stays for 4 × `debounce_polls` polls (1 hour at 3) is told anyway, because that is an outage rather than a flap. |
| `ignore` | empty | Globs on the alarm name that are never told. `*` matches any characters, slashes included, and `?` matches one character. Ignored alarms are still counted in the loop's health ("N ignored"). |

**Never held:**
- An account's first read. What it finds is told as backlog, as it was before the gate.
- An alarm the gate has not seen before, if it is in ALARM.
- The first poll after the gate ships. That poll publishes exactly as before the gate and records where every alarm stands.
- Anything about an alarm whose ALARM was told, until it is back to OK. Going to INSUFFICIENT_DATA does not count as back: a return to ALARM from there is told, and so is its eventual OK, including an OK reached by way of INSUFFICIENT_DATA.

**Flips held back:**
- The next event told for the alarm says how many there were: "It has flipped 14 times since the last report.", also in `flips_since_report` in its data.
- The daemon logs each one at info level as `aws-alarms: flips held back` with the alarm's name and the count. Nothing else is logged.

**Where the state is kept:** in the KV store (group `aws-alarm-gate`), one record per account and region. A restart therefore neither forgets how long an ALARM has lasted nor tells it twice.

**Target-tracking alarms:** ECS service auto scaling creates alarms named
`TargetTracking-service/<cluster>/<service>-AlarmHigh-<id>` and `-AlarmLow-<id>`. They are
marked DO NOT EDIT OR DELETE, and they flip whenever scaling does its job. To hide them:

```yaml
sources:
  aws:
    alarms:
      ignore:
        - "TargetTracking-*-AlarmHigh-*"
        - "TargetTracking-*-AlarmLow-*"
```
