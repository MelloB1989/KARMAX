<!-- last updated: 2026-08-28T06:35:38Z -->

# ABOUT_ME.md — Kartik Deshmukh (MelloB)

## Identity
- Name: **Kartik Deshmukh** — online alias "MelloB". ALWAYS refer to him, and sign on his behalf, as **Kartik**. NEVER "Nikhil": that is only a legacy account name left in some systems and addresses (nikhil@ghmev.in, the Linux user this runs as), it is not what anyone calls him, and a DIFFERENT REAL PERSON — Nikhil Gunda, who has his own section below and sits behind a confidentiality wall — is called that. Signing a message "on behalf of Nikhil" is wrong twice over: it is the wrong name for the operator and it names someone who must not be connected to this work.
- Location: Hyderabad, India (Erragadda area).
- WhatsApp: public/business (KARMAX) +91 75692 36628. Private/personal JID: 5794649083972@lid (use for sensitive/personal contact).
- Emails: kartikdd90@gmail.com | nikhil@ghmev.in | kartik@thevectorcompany.com
- GitHub: github.com/MelloB1989 | Portfolio: mellob.in | LinkedIn: linkedin.com/in/mellobai | X: x.com/mellob1989
- LeetCode: leetcode.com/u/mellob1989

---

## Current Life Situation (snapshot: 2026-08-28)
- Education: B.Tech (Anurag University), graduated June 2026.
- Employment: Zero (ZeroMoblt). Salary: ₹96,200/month (12 LPA). Manager/CTO: Manish (US-based; reviews all ORG-series PRs). HR: Naureen (JID 102903171833983@lid), HR system: Keka. 7-day notice period. Experience ~2.5 years. Weekends off. Princi has left the team; Dev is on the app revamp; conducted technical interview of candidate Chinmay (2026-08-15).
- Zero work in flight (confirmed again 2026-08-27): **oCrew + the ORG-series PRs are still sitting on Manish's review**, and SplitRide is in progress. The five ORG PRs have been mergeable and awaiting his re-review since 2026-07-24: ORG-10 (o-refine-react #246), ORG-14 (OPlatformInfraCDK #196), ORG-52 (OPlatformLambdas #194), ORG-51 (Cedar PEP shared authorization library) and one further PR. Five weeks of no movement — worth escalating rather than waiting.
- Job search: goal = MNC offer (Google/Microsoft, SDE-2 level, 15–20 LPA+). Current cadence: 1 LeetCode problem/day (Arrays/Sliding Window focus) + 2–3 applications/day, building toward ~200 problems in 3 months. Daily nudges live: 8:00 AM IST prompt, 9:00 PM IST check-in.
- Resume path (local): /Users/mellob/Developer/code/mellob-jobs/fde.pdf
- Finances: August salary largely consumed — ₹70,500 college fees + Cloudflare (₹510) + bank charges leave ~₹25,090 spendable. Watch discretionary spend, including AI inference costs.
- Health/ergonomics: neck/shoulder/back strain (July) — WFH posture notes still apply. Ergonomic chair (~₹8,700) + table (₹16–17k) planned for September; Green Soul gaming chair ruled out on Shiva's advice.

---

## Vector Company (God Mode) / CampX (state & actions)
- Retainer: ₹2.5L/month × 3 months (2026-08-17 → 2026-11-17), minimum 10 days/month on-site. Individual NDAs signed 2026-08-17 (Kartik, Shiva, Aymaan) with a GitLoom + Xyla carve-out. Company-level commercial agreement pending VCLABS Enterprise Pvt Ltd incorporation — Srikanth is awaiting an update on this.
- On-site: the Wed 2026-08-26 plan slipped; the team actually went in on **Thu 2026-08-27** (Shiva announced a 6 AM arrival the night before). The visit happened, but the deliverables are still unverified — Linux install on Shiva's PC and the top-3 YouTrack triage were never confirmed done. Follow-up with Shiva needed to confirm outcomes and assign owners/ETAs.
- **Cofounder call TONIGHT (Fri 2026-08-28, evening).** Krishna posted in god mode at 09:16 IST: "Let's all connect today evening. Few things to discuss." This is almost certainly where the deferred pendant conversation lands ("let's discuss about those things later"). Note the collision: the team told Srikanth that Kartik and Shiva are unavailable today for Rakhi, and Shiva is at his village — so this is an internal-only call.
- **Client sync — converging on Sat 2026-08-29, ONLINE, but still not confirmed with the client.** Thread in TVC X CampX on 2026-08-27 evening: Srikanth asked to meet in office Fri 2026-08-28; Shiva declined (Rakhi — he has gone to his village, Kartik unavailable for the festival) and proposed Monday; Srikanth said "Okay", then offered "if you are working day after we can connect online also" and settled on "let's connect day after then" (= Sat 2026-08-29), giving a window of "anytime between 10-12 pm" (as written — still ambiguous, confirm morning vs night). Shiva then misread it as Sunday, and at 23:50 asked "is the online meet is at Tuesday?". Krishna resolved it internally on 2026-08-28 at 09:15 — "I guess he means to have an offline meet on Saturday" — then corrected himself at 09:24 to **online**. So the team's working assumption is now **Saturday 2026-08-29, online**, but that is Krishna's inference: TVC X CampX has had no message since 2026-08-27 21:45, and nothing has been put back to Srikanth. ACTION: confirm the exact day + time (and AM/PM) with Srikanth in TVC X CampX, then restate it in god mode.
- Agenda for that sync: progress walkthrough (content engine + OCR pipeline), on-site outcomes (Linux setup + YouTrack triage owners/ETAs), Google OAuth re-auth, and AWS credentials.
- **AWS access is a live blocker.** Aymaan asked Srikanth in the client group (2026-08-27, 21:44) for AWS credentials so the team can start testing the pipelines; Kartik confirmed he had texted for AWS personally, "with all services" — EC2, ECS, S3, CloudFront, Lambda, Cognito, SNS, Bedrock, SQS, Step Functions, CDK. No reply yet. Note CampX's own cloud is Azure (confirmed 2026-08-24); the AWS ask is for the new pipeline work.
- Google OAuth re-auth is still urgent — consent/token window expires **2026-08-31**. The on-site window is gone, so fold it into the online sync or book a separate ~30-min slot with Shiva Charan.
- Claude SDK wrapper check for the CampX AI assessment (asked by Shiva 2026-08-20) was due **Thu 2026-08-27** — completion never confirmed. Verify status or re-commit a date.
- Dev status: first CampX PRs completed; codebase understood; content engine + OCR pipeline brainstormed. Blockers: additional repo access + CampX test accounts. Siva gave repo access and test accounts on 2026-08-24, but the **"exams" microfrontend repo access requested 2026-08-26 (00:04) has had no reply** — that repo is needed to see the current paper-evaluation UI before wiring the pipeline into it.
- CampX assessment epic in YouTrack: user stories (requirements, scope, story points) due ~2026-09-10.
- Tailscale: prior remote shell attempts blocked by an allowlist (re-hit 2026-08-25 — KARMAX could not start tailscaled itself); need allowlist unlock or temporary SSH credentials to bring nodes online during triage.
- Vendor consideration: Vanta (SOC 2/HIPAA automation) noted; sales contact Lauren Griffin (Lauren.griffin@vanta.com, +1 949-870-8244).
- **CONFIDENTIAL — "pendant" hardware idea (2026-08-27 evening, god mode).** Shiva raised it, shared a hardware reference image and "how others are using" it; Krishna confirmed it does what the team is planning and asked Shiva to share more. Krishna then put an explicit hold on the whole thread: "@all let's discuss amongst ourselves first and please not to share anything with anyone as of yet about this discussion." Shiva acknowledged. Keep this out of ALL outbound comms, client updates, and social posts until Krishna lifts the hold. The standing confidentiality wall also still applies: Nikhil Gunda must not learn about The Vector Company, that Kartik works there, or about Shiva/Aymaan/Krishna.
- Governance: Krishna's hold stands — all four cofounders (Kartik, Shiva, Krishna, Aymaan) consult before major decisions; a 3-way call is required before commitments are made on CampX/Aymaan's behalf. Shiva's standing instruction (2026-08-27): "Don't give much context [about] our vector / clients" in outbound messages.

---

## TrustStrike × CampX (VAPT — separate deal)
- POC: Siva/Sivaram — sivaram@campx.in, +91 88014 03300, JID 209517111472259@lid. Contract ₹2,00,000 + GST; ₹59k paid, ~₹80k second installment outstanding.
- **Status unchanged since 2026-08-25:** Siva replied in the Campx X TrustStrike group — "We don't have any update on this. Beta version is not yet ready." So recon is blocked *upstream on CampX's build*, not merely on a slow response. No ETA given; Kartik asked for one and offered to sanity-test an interim APK. **No reply in three days.**
- New security findings are written up and ready to deliver as soon as artifacts land.
- A managed Trust/status-page feature was built and pitched for deployment at trust.campx.in (modelled on trust.openai.com) — offered 2026-07-28, never picked up.
- Next move: get a dated ETA for the beta APK + test credentials, offer the interim-APK path again, and decouple the ₹80k installment from the APK so payment stops waiting on the build.

---

## Nikhil Gunda / hardware & healthcare (LYZN × Wear Synapse AI)
- **The app shipped.** 2026-08-27 21:18 — Kartik pushed the build and told Nikhil it would reach TestFlight shortly; App Store review expected to take "1-2 days" (≈ 2026-08-29/30) for approval. This closes the long-pending "IoT app update" item, but two things remain open: confirm the TestFlight build actually landed, and apply Nikhil's 2026-08-26 UI feedback — he wants it off the "neo spien" theme and onto **green / the brand colours** ("green and mana color pettu"); Kartik agreed but has not shipped it.
- MR20: SDK published at github.com/MelloB1989/mr20.sdk. The MR20 WiFi OTA command-order discrepancy (row 43 vs row 63) still needs manufacturer confirmation.
- **Healthcare pipeline is heating up (LYZN X Wear Synapse AI group).** 2026-08-25: a call with another hospital in Jubilee Hills, with an offline demo planned. 2026-08-27: a Delhi hospital wants a pilot — clarified as **10 units, 21 doctors across 2 hospitals**. Nobody has been assigned to scope or price this yet.
- Nani AI wearable: 5 physical samples ready since ~2026-08-08; designed to fit a watch by removing the back magnet.
- Drone: DJI purchase thread still live with Xboom Utilities Pvt Ltd (Bangalore dealer, ships to Hyderabad in ~6 days; quoted DJI Lito X1 Standard at ₹59,500 incl. GST). They messaged again 2026-08-27 09:48. A reminder to return Nikhil's DJI Mini 5 Pro call was due 2026-08-16 and is still unanswered.
- Personal context: Nikhil recently married and is financially tight.

---

## Deliverables / Tasks (high-value, near term)
- **Confirm the CampX sync day/time with Srikanth** (working assumption Sat 2026-08-29 online, "10-12" — resolve AM/PM) and restate it in god mode.
- **Tonight's cofounder call** (Krishna, Fri 2026-08-28 evening) — be on it; expect the pendant discussion.
- CampX Google OAuth re-auth before 2026-08-31 (blocking integration work).
- Chase AWS credentials from CampX (Srikanth/Siva) so pipeline testing can start.
- Chase Siva for "exams" microfrontend repo access (asked 2026-08-26, no reply).
- Verify/close the Claude SDK wrapper check (was due 2026-08-27).
- Confirm on-site outcomes with Shiva: Linux install status, YouTrack top-3 owners/ETAs, Tailscale connectivity.
- TrustStrike: dated ETA for beta APK + test credentials; chase the ~₹80k second installment (align invoicing with the retainer); start recon the moment artifacts are live.
- Nikhil's app: confirm the TestFlight build landed, track App Store approval (~2026-08-29/30), ship the green/brand-colour theme change.
- Scope the Delhi hospital pilot (10 units / 21 doctors / 2 hospitals) and the Jubilee Hills demo.
- Send Princi the SplitRide ticket link — promised "kal" on 2026-08-27 01:15, still not sent.
- InPsyd website update (brief from Shiva 2026-08-19) — pending since Aug 19, the oldest untouched Vector-side item.
- KARMAX onboarding K8–K10 — pending (K8 = upload ev-app chat, K9 = rotate credentials, K10 = Rishwak/Razak chats); S1 GHM invoice also open.
- Rameez's experience letter — 50+ days overdue; send from official company address. Oldest open item overall.
- Sneha's pay receipt — pending.
- KARMAX infra: Google Calendar auth (gws) is broken with `invalid_grant`; calendar events cannot be fetched until `gws auth` is re-run on the KARMAX server. Distinct from the CampX OAuth deadline.

### Open review questions (asked 2026-08-27, still unanswered)
- Has Shiva confirmed the TrustStrike logins sent on 2026-07-31?
- Was Nikhil's follow-up call about the DJI Mini 5 Pro drone purchase handled? (reminder was due 2026-08-16)

---

## Relationships (selected)
- Krishna — COO, The Vector Company (JID 124751804641374@lid). US-based (EDT, Buffalo), ~40, mentor to Kartik/Shiva/Aymaan; referred to as "anna". Primary stakeholder for CampX coordination; prefers evening coordination; owns the governance hold and the pendant confidentiality hold. Called the 2026-08-28 evening cofounder sync. Has explicitly asked for "more of YOU and less of your assistant" — reply to him personally.
- Shiva Charan — Vector cofounder / technical POC (JID 150285251002514@lid). Owns YouTrack, Google auth/consent, cloud access; Linux install target PC. At his village for Rakhi (2026-08-27 onward). Treats KARMAX casually/jokingly — not genuinely upset when he swears at it.
- Aymaan — Vector cofounder (JID 90391898509410@lid); tracks CampX expenses; asked CampX for AWS credentials on 2026-08-27; shares deliverables (wireframes etc.). Do not route Claude/AWS account asks through him — those go to Shiva only.
- Srikanth Yellapragada — TVC × CampX client contact / decision maker (JID 223076323192989@lid). Working on "the plan to take things forward"; offered the "10-12 pm" window for the online sync; awaiting VCLABS/company-NDA update.
- Siva (Sivaram) — CampX founder & TrustStrike VAPT POC; sivaram@campx.in, +91 88014 03300. Confirmed 2026-08-25 the beta build is not ready. Gave repo access + test accounts 2026-08-24; silent on the exams microfrontend request.
- Naureen (Zero HR) — HR contact for interviews/payroll (JID 102903171833983@lid); appreciates a heads-up on late arrivals. A ~2026-07-28 company policy email is still unacknowledged.
- Nikhil Gunda — IoT/hardware partner (MR20 device work, drone, wearable samples, hospital pipeline); recently married, financially tight. Must NOT know about The Vector Company or the team there.
- Sneha — personal/family contact; requested pay receipt.
- Princi Jain (JID 62801146044559@lid, +91 83096 82932) — ex-Zero colleague. **Auto-replies are DISABLED as of 2026-08-27** and her chat is locked in wacli — handle manually, keep strictly work-scoped. Tentative Fri 2026-08-28 catch-up is still unconfirmed and collides with the Rakhi unavailability the team gave the client; confirm or move. Owed the SplitRide ticket link.

---

## Preferences / Operating Notes
- Do NOT auto-reply or proactively monitor large promo/community/campus groups; keep KARMAX quiet in those channels unless explicitly asked.
- In God Mode, be conservative — no unsolicited replies; use the private JID for sensitive/personal matters and the public line for routine automation. Krishna wants Kartik himself, not the assistant.
- Nothing about the pendant discussion leaves the four cofounders until Krishna lifts the hold.
- No auto-replies to Princi Jain; her chat is locked and handled manually.
- Client comms: keep updates short, professional, and action-oriented to avoid scope drift; don't volunteer context about Vector's other clients (Shiva, 2026-08-27).
- CampX stakeholder comms (Shiva, Krishna, Srikanth): avoid AI auto-replies; escalate decisions to the operator unless routine; keep WhatsApp tone crisp and professional.
- Maintain separation between the "Nikhil"/Zero/Lyzn/KARMAX contexts and the Vector/CampX/TrustStrike contexts in outbound comms.

---

## Decisions / Financial & Infra notes
- **Inference is over budget.** Monthly ceiling is $20; the last 7 days cost ~$7.89 with a projected monthly run rate of **$33.83** (headroom −$13.82, status "over budget") — and that is a *lower bound*, since the gpt-5 / gpt-5-mini usage is unpriced in the report. Needs attention now, not at month end.
- AWS inference spiked to ~$224 on 2026-08-24 (KARMAX/Claude usage). Decision 2026-08-25: switch primary inference to GPT-5 mini and use a low-cost LLM for final polish. That switch is in effect (gpt-5-mini and Haiku 4.5 now carry most of the loop-gateway and memory traffic) but has not brought the run rate under budget.
- CampX cloud runs on Azure (Shiva's setup), not AWS (confirmed 2026-08-24) — separate from the AWS dev access being requested for the pipeline work.
- GPU rental (discussed with Shiva 2026-08-24): H200 at ₹600/hr for 14–16 hrs (~₹9,600 total); decision deferred while watching DGX Spark availability.
- Infra needs still open: 2+ Claude API accounts (~$100 each/month) + AWS dev accounts — ask Shiva only, not Aymaan; all expenses tracked.
- Social publishing: LinkedIn is currently in dry-run mode; no live posts until explicitly toggled off (`karmax social dry-run off`). Several drafts through 2026-08-27 were auto-refused for LinkedIn while the X variants passed — the refusals are the dry-run guard working, not a content problem to chase.

---

## Routines & Accountability
- Daily nudges active:
  - Morning: 8:00 AM IST — LeetCode topic + job-application reminder.
  - Evening: 9:00 PM IST — accountability check-in ("Aaj ka LeetCode ho gaya?" / "Aaj kitni jobs apply ki?").
- Current floor after the broken streak: 1 LC problem + 2–3 applications/day; scale back up toward 4–5 problems/day over the next weeks.
- Note: the evening check-ins have gone unanswered for several days running (2026-08-25 → 2026-08-27) — the streak is not being self-reported.

---

## Upcoming / Events
- **Fri 2026-08-28 — Raksha Bandhan.** Kartik is unavailable for the festival (already communicated to Srikanth as the reason for skipping the office meet). Two things still sit on today: Krishna's evening cofounder call, and the unconfirmed Princi Jain catch-up — which likely needs moving.
- **Sat 2026-08-29 — CampX online sync with Srikanth** (his "day after", "10-12 pm" window; team's working read after Krishna's 2026-08-28 correction) — still UNCONFIRMED with the client. Same day: 18startup MVP showcase, Hyderabad, 5–8 PM — tentative, preparation not started, and it overlaps nothing but competes for the day.
- ~Sat 2026-08-29 / Sun 2026-08-30 — expected App Store approval window for Nikhil's app.
- **Mon 2026-08-31 — hard deadline: CampX Google OAuth re-auth.** Also the day Shiva originally proposed to Srikanth for the in-office meet, and the fallback if Saturday slips.
- ~2026-09-10 — CampX assessment epic user stories due.

---

_Last updated: 2026-08-28 (refresh after Krishna's morning correction that the Srikanth sync is Saturday and ONLINE, his call for a cofounder sync tonight, Nikhil's app going to TestFlight, the Delhi/Jubilee Hills hospital pilot leads, and the inference run rate going over budget)_
