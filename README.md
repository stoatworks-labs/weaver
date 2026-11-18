# weaver — multi-WAN bonding with adaptive failover for OpenWrt

> **AI-assisted project.** This codebase was created with [Claude Code](https://claude.com/claude-code)
> (Anthropic), directed and reviewed by a human author. **Work in progress: nothing here
> has run on a router yet.**

One Go binary, `weaver`, that bonds every WAN a router has (Ethernet, hotel Wi-Fi,
USB tethering, a cellular modem) into one encrypted tunnel to a remote endpoint.
It runs as either end: on a GL.iNet travel router as an OpenWrt package, or on a
VPS as a container.
