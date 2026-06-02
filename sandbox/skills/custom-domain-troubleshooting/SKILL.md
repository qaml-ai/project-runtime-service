---
name: custom-domain-troubleshooting
description: Diagnoses and resolves custom domain issues including SSL errors, pending activation, DNS misconfigurations, 522s, and apps not loading on their custom domain. Use when users ask about custom domain setup or troubleshooting.
---

# Custom Domain Troubleshooting

camelAI custom domains are configured per deployed app. Each app can have one exact custom hostname, such as `www.example.com`, `app.example.com`, or `example.com`.

## How It Works

- Users choose the exact hostname they want for a specific app.
- Wildcards are not supported.
- A hostname can only be assigned to one app at a time.
- Apex/root domains are allowed if the user's DNS provider supports CNAME-like flattening/ALIAS/ANAME at the apex.
- camelAI provides the DNS target. Users should not invent their own target.
- Cloudflare provisions one custom hostname certificate per exact hostname.
- SSL validation uses HTTP validation through Cloudflare for SaaS. Do not ask users to add `_acme-challenge` or other DCV records.

## Required DNS Record

There is one DNS record per custom hostname:

| Field  | Value |
|--------|-------|
| Host   | The exact hostname the user chose, for example `www.example.com` or `app.example.com` |
| Type   | `CNAME` |
| Target | Shown by `get_custom_domain` as `dns_target`, normally `custom-domains.camelai.app` |

For apex/root domains like `example.com`, many DNS providers do not allow a normal CNAME at the root. In that case tell the user to use the provider's CNAME flattening, ALIAS, or ANAME feature pointing to the same `dns_target`. If their provider has no apex aliasing feature, they should use a subdomain such as `www.example.com`.

## When the Custom Domain URL Works

The custom domain should work when all of these are true:

1. The app has a custom `hostname` configured.
2. The hostname's DNS resolves to the `dns_target`.
3. The Cloudflare hostname `status` is `active`.
4. The Cloudflare `ssl_status` is `active`.

Until then, the app still works on its default `*.camelai.app` URL.

## Diagnostic Workflow

### Step 1: Call `get_custom_domain`

This returns:

- `dns_target` — the target all custom hostnames should point to
- `apps[]` — each app's configured hostname, Cloudflare hostname status, SSL status, error, and DNS check
- `apps[].dns_checks.routing_cname` — live DNS resolution for that exact app hostname

Each DNS check has `status = "ok" | "mismatch" | "missing" | "unavailable"`. `unavailable` means the diagnostic check failed; do not claim the user's DNS is wrong based only on that.

### Step 2: Follow the Decision Tree

```
Does the app have a custom hostname?
├─ No → Ask which exact hostname they want for which deployed app, then use set_custom_domain.
└─ Yes
   ├─ dns_checks.routing_cname.status = "missing" | "mismatch"?
   │  └─ Tell them the exact record to add or fix:
   │     {hostname} CNAME {dns_target}
   │
   ├─ dns_checks.routing_cname.status = "unavailable"?
   │  └─ Show the expected record and ask them to verify it manually at their DNS provider.
   │
   ├─ status != "active" or ssl_status != "active"?
   │  ├─ If recently configured → wait a few minutes, then recheck.
   │  ├─ If stuck for a while → use retry_custom_domain_hostnames.
   │  └─ If still stuck → inspect the app error from get_custom_domain.
   │
   └─ status = "active" and ssl_status = "active" and DNS check is ok?
      └─ The custom domain should work. If the user still sees an error, ask for the exact browser error/status code.
```

### Step 3: Give the Specific Fix

Always provide the exact DNS record values from `get_custom_domain` or `set_custom_domain`. Do not tell users to add wildcard records. Do not tell users to add `_acme-challenge` records.

## Common Errors and Fixes

| Error | Likely cause | Fix |
|-------|--------------|-----|
| `DNS_PROBE_FINISHED_NXDOMAIN` | Hostname DNS is missing | Add `{hostname} CNAME {dns_target}` |
| Browser shows SSL/certificate error | Cloudflare SSL is not active yet or hostname does not resolve to the target | Run `get_custom_domain`; fix DNS or retry provisioning |
| `ssl_status: "pending_validation"` for more than a few minutes | DNS not pointed at the target, or Cloudflare validation is stuck | Verify DNS, then use `retry_custom_domain_hostnames` |
| Error 1014 | Customer domain is on Cloudflare and CNAME is proxied in a conflicting way | Set the CNAME to DNS-only if needed, then retry |
| HTTP 522 | Request reached Cloudflare edge but did not reach the dispatcher/origin path | Check DNS target and escalate if DNS and SSL are active |
| App works on `*.camelai.app` but not custom domain | Custom hostname not fully active or DNS mismatch | Check that app's `status`, `ssl_status`, and DNS check |

## DNS Provider Notes

- **Cloudflare DNS**: For subdomains, use a CNAME to the target. If proxied mode causes 1014 or validation issues, switch the record to DNS-only.
- **Apex/root domains**: Use CNAME flattening, ALIAS, or ANAME if the provider supports it. Otherwise use a subdomain like `www`.
- **Namecheap / GoDaddy / Route53 / Google Domains**: Use a normal CNAME for subdomains. For apex domains, use the provider's apex aliasing feature if available.

## Available Tools

| Tool | Purpose |
|------|---------|
| `get_custom_domain` | Full diagnostic: DNS target, live DNS checks, per-app hostname/SSL status |
| `set_custom_domain` | Set or change one exact custom hostname for one app, admin only |
| `remove_custom_domain` | Remove the custom hostname from one app |
| `retry_custom_domain_hostnames` | Re-provision failed or stuck Cloudflare hostnames |

## Escalation

If DNS points to `dns_target`, Cloudflare status and SSL are active, and the domain still fails, ask the user for the exact hostname and error code. Include the `get_custom_domain` output when escalating.
