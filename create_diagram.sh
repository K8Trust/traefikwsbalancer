#!/bin/bash

# Create a text-based diagram that we'll convert to an image
cat > diagram.txt << 'EOF'
┌────────────────────────────────────────────────────────────────────────────────────────┐
│                     Traefik WebSocket Connection Balancer                              │
├────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                        │
│   ┌───────────┐          ┌──────────────────────────────┐         ┌─────────────────┐  │
│   │           │          │       Traefik Proxy          │         │                 │  │
│   │  Client   │  ──────> │ ┌────────────────────────┐   │ ──────> │ Metrics         │  │
│   │ Browser/  │  WebSock │ │ WebSocket Connection   │   │  JSON   │ Endpoint        │  │
│   │   App     │          │ │      Balancer          │   │         │ /balancer-      │  │
│   │           │          │ └────────────────────────┘   │         │ metrics         │  │
│   └───────────┘          └──────────────────────────────┘         └─────────────────┘  │
│                                        │                                                │
│            Routes to service with lowest connection count                               │
│                                        │                                                │
│                                        ▼                                                │
│   ┌─────────────┐       ┌─────────────────────────┐      ┌─────────────────┐           │
│   │ Backend 1   │       │     Backend 2           │      │   Backend 3     │           │
│   │ ┌─────┐┌───┐│       │ ┌─────┐┌────┐┌─────┐    │      │ ┌─────┐┌─────┐  │           │
│   │ │Pod 1││Pod││       │ │Pod 1││Pod2││Pod 3│    │      │ │Pod 1││Pod 2│  │           │
│   │ │     ││ 2 ││       │ │     ││    ││     │    │      │ │     ││     │  │           │
│   │ └─────┘└───┘│       │ └─────┘└────┘└─────┘    │      │ └─────┘└─────┘  │           │
│   └─────────────┘       └─────────────────────────┘      └─────────────────┘           │
│        │                           │                             │                      │
│        │                           │                             │                      │
│        │                           │                             │                      │
│        └───────────────────────────┼─────────────────────────────┘                      │
│                                    │                                                    │
│                                    ▼                                                    │
│   ┌────────────────────────────────────────────────┐   ┌───────────────────────────┐   │
│   │        Connection Count Collection             │   │     Pod Discovery         │   │
│   │  1. Direct Service Metrics (/metric)           │──>│     Methods:              │   │
│   │  2. Pod Discovery via IP Scanning              │   │  1. Endpoints API         │   │
│   │  3. Connection Count Caching                   │   │  2. Direct DNS            │   │
│   └────────────────────────────────────────────────┘   │  3. IP Scanning (±5 3rd   │   │
│                                                        │     octet, e.g. 100.68.X.Y)│   │
│                                                        └───────────────────────────┘   │
│   IP Scanning finds pods across different subnets                                      │
│   Example: Discovers 100.68.32.4 and 100.68.36.4                                       │
└────────────────────────────────────────────────────────────────────────────────────────┘
EOF

# Display the diagram in the terminal
cat diagram.txt

# Try to convert to PNG if possible using terminal2image
if command -v npm > /dev/null; then
  npm install -g terminal-to-image
  terminal-to-image diagram.txt diagram.png
  echo "Diagram saved as diagram.png"
else
  echo "Diagram saved as diagram.txt. Install terminal-to-image via npm to convert to PNG"
fi 