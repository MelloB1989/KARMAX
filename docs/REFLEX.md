# Reflex: System One in front of the orchestrator

## The problem

Every event reaching KARMAX cost a full turn on a coding harness. A WhatsApp
"👍", a timer firing, a webhook from a service that pings hourly — each one woke
Claude Code, built six blocks of context, and spent minutes and a real token
bill arriving, usually, at *nothing needs doing*.

The decision was never the expensive part. Making it with a conversational model
was.

## The shape

Reflex puts TypeSafe's Jev — a System One model that returns calibrated
probabilities instead of prose — in front of the brain. One evaluation per event
answers the whole decision sheet at once:

| Question | Type | What it settles |
|---|---|---|
| `disposition` | choice | drop / remember / handle / delegate / escalate |
| `urgency` | score | how soon it must be dealt with |
| `risk` | score | what acting on it costs if wrong |
| `remember` | noul | whether it states a durable fact |
| `effort` | choice | trivial / normal / heavy |

**Only input tokens are billed**, and every question is answered in one pass
against one state. So a question that is *sometimes* useful costs almost nothing
to *always* ask — which is why this is one call and not five, and why adding a
sixth question later is close to free.

The answers are numbers, so the decision lives in `Decide()` — ordinary Go, no
network, no clock — rather than in a sentence somebody has to parse.

## Where it sits

```
bus.Consume(SubAgentRouter)      ← serial, one event at a time
  └─ openTurn                     (journal row)
      └─ agent.Send → mailbox     ← concurrent per conversation
          └─ handleOne
              ├─ screen()         ← REFLEX
              │    drop     → finish the turn, no model
              │    remember → file it, no model
              │    else     → annotate the event and carry on
              └─ handleEvent
                   └─ brainFor()  ← trivial → API session, not the harness
```

Screening runs on the **mailbox worker, not the router**. That placement is
load-bearing: `bus.deliver` hands events to a subscriber one at a time, so a
network call in the router would put every screening in series behind every
other — a burst of twenty events would pay twenty screenings end to end. On the
mailbox worker they run concurrently across conversations and stay ordered
within one.

The cost is one journal row per dropped event, which is honest bookkeeping
anyway: deciding an event needed nothing *is* handling it, and the turn closes
as `ok`.

## The two savings

**Drops and files.** An event screened out never reaches a model. `karmax reflex
stats` reports this directly as the share of events that never reached the brain.

**Effort routing.** This is the larger one. Before, whenever a harness brain was
wired, *every* turn went to it. A `trivial` verdict now routes to the API
session instead — seconds instead of minutes. `brainFor` is deliberately
conservative: only an explicit `trivial` verdict redirects, and only when there
is both a harness and an API session, so anything unscreened or unsure lands on
the engine it always used.

`delegate` and `escalate` do not change the engine; they change what the turn is
told, through `Screening.context()`.

## The safety properties

These are the reasons this is safe to put on the hot path at all.

1. **It never fails closed.** No key, a timeout, a 429, a 529, an unreadable
   answer, an unknown disposition — every one returns "handle it the old way",
   marked `FailedOpen`. A `FailedOpen` verdict annotates nothing, so a turn is
   never told reflex decided something when reflex never ran.
2. **The operator is floored.** Nothing from an operator chat is ever dropped or
   merely filed. Reflex chooses *how* their message is handled, never *whether*.
   Operator status is established from the chat it arrived on, never from
   anything the model said.
3. **A drop must clear a bar.** Dropping is the only irreversible verdict, so it
   is the only one with a confidence threshold. An unsure drop becomes a handle.
4. **Nothing worth keeping is discarded.** A drop that also scores high on
   `remember` is demoted to `remember`, not dropped.
5. **An unknown chat is the operator's.** Mirrors `Agent.isFromOperator`, so an
   unrecognised id is never silently screened away.
6. **A breaker.** Three consecutive failures pause screening for 30s, so a
   TypeSafe outage costs one timeout per cooldown rather than one per event.

## Calibration

Thresholds are model-version specific — an alias moves under you and the cuts
move with it. Every verdict is stored with the probabilities behind it, so a
threshold can be re-cut after the fact without re-running anything.

```
karmax reflex stats --days 7
karmax reflex recent --action drop --limit 40
```

Read `recent --action drop` first. If anything in that list should have been
answered, raise `thresholds.drop`.

## Loops

A loop that wanted a judgement had only `Kit.Ask`, which spends a full agent
turn. Most loop questions are small — is this worth replying to, which lane does
it belong in — and a turn is the wrong instrument for a small question.

`Decide` gives a loop the same model, with its **own** questions:

```go
d, err := loopwasm.Decide(message, loopwasm.Questions{
    "reply": loopwasm.Noul("Does this need a reply from the assistant?").
        When("It asks something or expects action", "Small talk or an acknowledgement"),
    "lane": loopwasm.Choice("Who should handle it?", map[string]string{
        "assistant": "answerable from what is already known",
        "operator":  "only they can decide",
    }),
    "heat": loopwasm.Score("How annoyed is the sender?", "calm", "impatient", "angry"),
})
if err != nil {
    return // unavailable is not a no — do whatever you did before
}
if !d.Yes("reply", 0.7) {
    return
}
if lane, conf := d.Choice("lane"); lane == "operator" && conf > 0.8 {
    loopwasm.Propose("Needs you", message, draft)
    return
}
```

An error means the model was unavailable, **not** that the answer was no. A loop
must treat it as "do whatever you did before this call existed".

Compiled-in loops use the same vocabulary through `loopkit.Kit.Decide`.

### Declaring it

`decide` is a host function, so a sandboxed loop must declare it and be
re-signed:

```yaml
host:
  - decide
capabilities:
  - tool:reflex.decide
```

Both gates apply: the manifest must list it, and the Broker must grant it.

## Configuration

See the `reflex:` block in `karmax.yaml.example`. With `enabled: false`, or with
no `TYPESAFE_API_KEY`, `reflex.New` returns a nil evaluator and every path
behaves exactly as it did before this package existed.
