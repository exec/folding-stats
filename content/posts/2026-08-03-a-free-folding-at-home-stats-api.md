---
title: A free Folding@home stats API
date: 2026-08-03
summary: Every donor, every team, updated hourly — with an open, unauthenticated API, and the whole thing open-sourced under MIT.
---

This site tracks every Folding@home donor and every team: points, work units, rankings, and production over time. It updates within about a minute of the upstream feeds publishing, roughly once an hour.

Everything on it is also available as JSON — no key, no account, no rate limit. If you want your own numbers on your own dashboard, [the API](/api) is the whole site.

## Why I built it

First, some credit where it is due.

**Folding@home** has been running distributed protein folding for a quarter of a century, and it publishes its complete statistics as public feeds that anyone may use. None of this would exist without that. The raw data is theirs, and they give it away.

**ExtremeOverclocking** has published Folding@home statistics for about as long, for free, and a good part of the community's competitive culture grew up around their pages. Running something like that is not free — a stats site of that size is a real bandwidth and database bill that somebody has quietly paid for two decades. More recently they added a browser challenge to their pages, which is a sensible way to keep automated traffic from eating a free service alive. Anyone who has watched scrapers hammer a public endpoint will understand the decision.

Their terms ask people not to pull from the site programmatically, and that is entirely their call to make — it is their bandwidth being spent. So this project has never touched their pages, and never will.

But the appetite for programmatic stats did not disappear when the challenge went up, and that appetite is what created the pressure in the first place. Every team Discord bot, every signature image, every leaderboard widget, all pointed at a site that was never built to be an API and was never asked to be one. What was missing was somewhere for that traffic to go *instead*.

That is the part I hope actually helps. If some of the bot and dashboard authors point at this site rather than at theirs, that is load lifted off a service that has carried it for free for twenty years. I would count that as a success on its own, separately from anything else here.

So the data comes from the same upstream source they use, and the derived layer is built here from scratch. Folding@home publishes cumulative totals; everything anyone actually wants — rates, rankings, history, per-team breakdowns — has to be worked out by comparing one snapshot against the next. That work has to happen somewhere. It may as well happen once, in public, and be given away.

## Why it can afford to be open

I learned to program in PHP in the mid-2010s and still have a soft spot for it. An enormous amount of the web runs on PHP, usually for very good reasons, and I would not talk anyone out of it.

It does mean a different set of tradeoffs under automated traffic, though. A request that starts an interpreter and goes back to the database is doing real work every single time somebody asks, and high-frequency callers ask constantly. Serving that generously gets expensive in a hurry.

This is written in Go and compiled, and — more importantly than the language — it keeps the entire recent dataset in memory. The figures a caller asks for are already computed and already resident, so answering is a lookup rather than a query. That is what makes it possible to leave the door open with no key and no quota: an automated caller is not costing much more than a person clicking around.

That caching is frankly experimental, and it will keep changing. I have already rewritten parts of it more than once, and I expect real traffic to teach me things a benchmark cannot. Performance work here is nowhere near finished.

So this is not a replacement for anything. It is the same underlying data, presented with fewer constraints and a fresher coat of paint, with the API I wanted to exist.

## The API is the point

I think this is the part that matters most.

A free, unauthenticated API for folding statistics is genuinely useful to the people doing the folding. Team captains can build their own recruitment and retention pages. Somebody can finally make a decent mobile widget. Discord bots stop screen-scraping. Rig monitoring dashboards can show what the rig is actually earning. And there will be uses nobody has thought of yet, which is usually the best argument for opening something up.

So: no key, no sign-up, no quota, and I intend to keep it that way for as long as I can keep the lights on. If you build something against this, I do not want to be the reason it breaks.

```
GET /v1/donors/{name}          one donor, with per-team breakdown
GET /v1/donors/{name}/history  production over time
GET /v1/teams/{id}/members     a team roster
GET /v1/summary                project-wide totals
```

Every response carries a `snapshot` block saying how fresh the data is and when the next update is due, so you can cache against it rather than poll. Full documentation is on [the API page](/api).

## Open source, MIT

The entire site is open source under the MIT license — the collector, the storage engine, the API, and this frontend.

That is deliberate. If your team would rather run its own instance, or point it at a different subset of the data, or fork it and take it somewhere I have not thought of, you should be able to. It also means this site is not a single point of failure: if I get hit by a bus, the thing that makes the numbers is still sitting there, and somebody else can stand it back up.

## Donors are their own entity

One thing here works differently, and it is a deliberate modelling choice rather than a correction.

Most team-oriented stats treat a donor as a member *of a team*. If you fold for three teams, you appear three times, with three separate totals and three separate ranks within those teams.

This site treats a donor as the primary thing. Your totals are the sum of everything folded under your name, your rank is against every other donor, and the per-team breakdown comes back in the same response. That matches how the official Folding@home statistics think about identity, and it answers "how much have I done" in one number.

An honest consequence: donor names are not unique and never have been. `Anonymous` appears on nearly six thousand teams and is clearly not one person. Where a name looks shared, the site says so on the page rather than presenting the aggregate as an individual.

## About the numbers

One naming note, because it trips people up. The figure commonly labelled "24hr Avg" on stats sites is, per the documentation, a seven-day moving average — the label is a historical artifact more than anything else. Here it is called `points_per_day_7d_avg`, because I wanted a field name that explains itself to somebody reading the API cold.

I did check the arithmetic before committing to it: rounding the seven-day total to the nearest point reproduces the published figures exactly, and truncating does not.

Anything bucketed by calendar day or month is explicitly UTC and says so. Timestamps that are moments rather than periods — publish times, hourly points — render in your own timezone.

## What is not here yet

**History starts on 3 August 2026.** Lifetime point totals come from upstream and are complete going back to the beginning. But *rates and history* — anything requiring a comparison between two moments — only exist from the day collection started. That window deepens every hour.

**No overtake projections.** These are a popular feature elsewhere and I understand the appeal. I have left them out because the projection assumes your output and your rival's both hold constant, which is not true of anybody with a job or a power bill. If enough people want it anyway, I will revisit.

**No affiliation.** This site is not run by Folding@home, Stanford, Washington University, or ExtremeOverclocking. The data comes from the official Folding@home feeds.

## What is next

Mostly just running — the site gets more useful every day it keeps collecting. Beyond that: deeper per-team member breakdowns, and whatever people ask for once they start building against the API.

If something is wrong, missing, or you need an endpoint that does not exist, please say so.
