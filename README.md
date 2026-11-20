# Counterfoil — frontend

**Named humans decide. Agents do the rest. Done means confirmed.**

> **TBO VoyageHack 4.0 · PS2 (AI-native HR services platform) · Team Init to Win It.**
> This repo is the product's **front door on Vercel** plus the complete **interface
> source** for review. The runnable service lives in
> [`counterfoil_backend`](https://github.com/NAME-ASHWANIYADAV/counterfoil_backend).

## How the two repos connect

Counterfoil's interface is deliberately **server-rendered**: the same Go process that
holds the receipts renders every page from them — one binary, no client framework, no
build step, nothing the audit can't see. So the split is:

```
browser ── https://<your-app>.vercel.app          (THIS repo, deployed on Vercel)
                    │   proxies every path, same origin —
                    │   sessions, forms and the live poller just work
                    ▼
          https://counterfoil-backend.onrender.com (backend repo, deployed on Render)
          renders the pages in ui/templates · runs agents, warrants, receipts
```

`vercel.json` here forwards every request to the backend. Because the browser only ever
talks to the Vercel origin, cookies stay first-party and nothing needed CORS.

## Deploy (two steps)

1. Deploy the backend on Render first — build `go build -o cfoil ./cmd/cfoil`, start
   `./cfoil demo`, env `CFOIL_AI_API_KEY`. Its README has the details (the service binds
   Render's `PORT` and keeps itself awake on the free tier by pinging its own
   `/healthz`).
2. Import **this repo** in Vercel (no framework, no build command) and put your Render
   URL into `vercel.json` — it ships pointing at
   `https://counterfoil-backend.onrender.com`; edit that one line if your service name
   differs. Deploy. The whole product is now live at your Vercel URL.

## What's in `ui/` (the interface source)

Mirrored from `counterfoil_backend/internal/webapp` so the frontend can be reviewed
where a frontend is expected — it compiles there, inside the backend binary:

```
ui/templates/   every screen as a Go template — chat, My exit, approvals with the
                "what your signature binds" card, case room with the 19-step tracker,
                in-doubt queue, controls & drills, packages, people, clocks, receipts,
                audit room, login, public pages
ui/static/      app.css — the whole design system in one file, built on TBO's measured
                public design language (Montserrat + #307FE2/#F36B21 from tbo.com; the
                portal's Open Sans, navy #014693, gold accents); app.js — a small
                poller and niceties, no framework; self-hosted SIL-OFL fonts. No CDN,
                no tracker, no TBO logo or mark.
ui/*.go         the page handlers and the two agent surfaces (leaver chat, in-doubt
                investigation) that the templates render
```

If you change the interface, change it in the backend repo (`internal/webapp/`) — that
is what ships; this mirror is refreshed from it.

## The product in one paragraph

Counterfoil is an agent-first HR services platform shown in depth on employee exits.
AI agents run the routine steps; six reserved decisions can only be made by named
people, each signature bound to the exact request by hash; and nothing counts as done
until the target system's own record confirms it — every state change leaving a
hash-chained, countersigned receipt that an auditor can re-check offline, no network,
no database. A reply is a claim; the record is the fact.

## Team

**Init to Win It** — Sonal Verma (lead), Ashwani Yadav, Arpita, Vaibhav Gupta,
Lakshay Bansal. All people and data in the demo are synthetic. AI tools: Claude
(Claude Code) was used for research, drafting and writing under the team's direction;
full disclosure in the backend README and the Round 1 architecture brief.
