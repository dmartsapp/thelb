# Diagrams

The [philosophy](philosophy.md) and the [design](design.md) in pictures. Each diagram is a summary; where it and the text disagree, the text is right.

## 1. How theLB treats a machine

The philosophy, as one flow: full authority or nothing, the running kernel as the truth, fail closed, and nothing left behind.

```mermaid
flowchart TD
    start(["thelb director or thelb backend starts"]) --> auth{"Full authority?<br/>capabilities, /proc/sys writable,<br/>IPVS, IPv6, Kubernetes permissions"}
    auth -->|"no"| refuse["Change nothing.<br/>Say what is missing and how to grant it.<br/>Exit 1"]
    auth -->|"yes"| read["Read the running kernel:<br/>parameters, addresses, IPVS, modules"]
    read --> each{"For each thing it needs:<br/>already right?"}
    each -->|"yes"| leave["Leave it alone.<br/>Never revert it: it is the user's"]
    each -->|"no"| change["Change it at runtime.<br/>Record the old value, log what and why"]
    leave --> run["Run"]
    change --> run
    run --> drift{"Watchdog loop:<br/>anything drifted?"}
    drift -->|"yes"| fix["Put it back, and log it"]
    fix --> drift
    drift -->|"no"| lease{"A director holding VIPs:<br/>lease from the majority still valid?"}
    lease -->|"no"| giveup["Give up every VIP: fail closed"]
    giveup --> drift
    lease -->|"yes"| drift
    run -.->|"SIGTERM"| stop["Hand over leadership, give up the VIPs,<br/>remove what it created,<br/>restore what it changed"]
    stop --> gone(["Nothing left behind -<br/>except the secret files of the first bootstrap"])
    run -.->|"needs to survive a reboot?"| gen["thelb generate prints it -<br/>the user decides whether to install it"]
```

## 2. The pieces

Directors form the Raft cluster; the leader holds the control VIP and the service VIPs. Backends are clients of the leader, never Raft members. Replies go from the backends straight to the clients.

```mermaid
flowchart LR
    clients(["Clients"]) -->|"to VIP:port"| net["L2 network"]
    net --> leader
    subgraph cluster["Directors: Raft over mutual TLS"]
        leader["Leader<br/>control VIP 10.0.0.10:7443<br/>service VIPs, IPVS, the CA"]
        d2["Director"]
        d3["Director"]
        leader <-->|"Raft"| d2
        leader <-->|"Raft"| d3
    end
    leader -->|"packet unchanged,<br/>to the backend's MAC"| web["Backend web-1<br/>VIP on thelb0"]
    leader -->|"packet unchanged"| db["Backend db-1<br/>VIP on thelb0"]
    web -.->|"heartbeat, attach, status<br/>mutual TLS, to the control VIP"| leader
    db -.->|"heartbeat, attach, status"| leader
    web ==>|"reply, from the VIP"| clients
    db ==>|"reply, from the VIP"| clients
    op(["Operator: thelb status, thelb admin"]) -->|"Unix socket"| d2
```

## 3. Starting a director

The same command on every director. What it does depends on its secret file and on what answers on the control VIP.

```mermaid
flowchart TD
    s(["thelb director --vip --port<br/>--secret-file --backend-secret-file"]) --> probe{"What answers on<br/>the control VIP and port?"}
    probe -->|"something that is not theLB"| taken["Refuse: the VIP or port is taken"]
    probe -->|"a theLB cluster"| hasfile{"Secret file exists?"}
    hasfile -->|"no"| nofile["Stop: no secret.<br/>Get it on a director with<br/>thelb admin secret show"]
    hasfile -->|"yes"| prove["Prove the secret over TLS"]
    prove -->|"refused"| stale["Stop: the secret is out of date,<br/>or another cluster's"]
    prove -->|"accepted"| pending["Wait, pending, until<br/>a director approves"]
    pending --> member["Certificate signed.<br/>Raft non-voter, then voter"]
    probe -->|"nothing, for the whole probe window"| boot{"Started with --bootstrap?"}
    boot -->|"no"| wait["Wait, and try again"]
    wait --> probe
    boot -->|"yes"| file2{"Secret file exists?"}
    file2 -->|"no"| write["Create the secrets.<br/>Write the files once: 0600, never overwritten"]
    write --> create
    file2 -->|"yes"| create["Create the cluster:<br/>a CA in memory, a Raft cluster of one,<br/>the control VIP taken and announced"]
```

## 4. Joining, and approval

A director and a backend join the same way; only what follows the approval differs.

```mermaid
sequenceDiagram
    participant N as New node
    participant L as Leader, on the control VIP
    participant D as Any approved director
    participant O as Operator
    N->>L: TLS connection
    N->>L: HMAC of the secret, over this connection
    L->>N: HMAC of the secret, over this connection
    Note over N,L: Both proved the secret. It never crossed the network.
    N->>L: name, addresses and MAC addresses, certificate request
    L->>L: check the address, the MAC against its neighbour table, the name
    L-->>N: pending
    O->>D: thelb status
    D-->>O: the pending table, with the key fingerprint
    O->>N: thelb status
    N-->>O: its own key fingerprint
    O->>D: thelb admin approve name
    D->>L: approve, under the director's own identity
    L->>L: record who approved, sign the certificate
    L-->>N: certificate
    alt a director
        L->>N: Raft snapshot, the CA included
        Note over N: a non-voter, then a voter once it has caught up
    else a backend
        N->>L: heartbeat every 2 s, and the services it serves
    end
```

## 5. A backend, from enrolment to traffic

```mermaid
flowchart TD
    e(["thelb backend --join 10.0.0.10:7443<br/>--secret-file --serve web=8443"]) --> enrol["Enrol with the backends' secret"]
    enrol --> appr{"Approved by a director?"}
    appr -->|"not yet"| p["Pending: listed in<br/>a director's thelb status"]
    p --> appr
    appr -->|"refused"| r["Stop"]
    appr -->|"yes"| cert["24-hour certificate.<br/>Its MAC pinned on the director"]
    cert --> allow{"Allowed to serve web?<br/>The allow-list, or a delegated pool"}
    allow -->|"no"| deny["Attachment refused, and logged"]
    allow -->|"yes"| prep["VIP on thelb0, ARP parameters set,<br/>port 443 mapped to 8443"]
    prep --> checks{"Its own check ok,<br/>the director's check ok,<br/>its lease current?"}
    checks -->|"yes"| on["In the IPVS service:<br/>gets new connections"]
    checks -->|"no"| off["Weight 0:<br/>no new connections"]
    on --> checks
    off --> checks
```

## 6. A packet

Direct return: the director sees only the client's side of the conversation.

```mermaid
sequenceDiagram
    participant C as Client
    participant D as Director holding the VIP
    participant B as Backend
    C->>D: SYN to 192.0.2.10:443
    D->>D: IPVS picks a backend, by Maglev hashing
    D->>B: the same packet, sent to the backend's MAC
    B->>B: the VIP on thelb0 accepts it, mapped to port 8443 if asked
    B->>C: SYN-ACK from 192.0.2.10:443, directly
    C->>D: the rest of the request
    D->>B: forwarded the same way
    B->>C: the response, directly
```
