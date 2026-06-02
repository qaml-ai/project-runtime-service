---
name: communication-channels
description: Use camelAI communication channels from js_exec, including email, Slack, Telegram, and the current workspace email address.
license: Complete terms in LICENSE.txt
---

# Communication Channels

Use this skill when the user asks to send or receive messages through email, Slack, Telegram, or another external channel.

Normal chat replies stay in chat. Sending through a channel is an external side effect. Only send externally when the user explicitly asks for it, or when a channel-originated turn includes hidden instructions to reply through that channel.

## Discovery

Channel send tools are available inside `js_exec`, not as top-level tools:

```js
await tools.help("communication")
```

For the current workspace email address:

```js
await env.WORKSPACE.emailAddress()
```

This returns the address string or `null`.

## Common Calls

```js
await tools.send_email({ to, subject, text })
await tools.send_slack_message({ integration_id, channel_id, text })
await tools.send_telegram_message({ integration_id, text })
```

Do not invent Telegram chat IDs. Outside Telegram-originated threads, pass `integration_id` when more than one Telegram connection exists.

Channel tools accept attachments by file path:

```js
attachments: [{ path: "/mnt/user-uploads/file.pdf" }]
```
