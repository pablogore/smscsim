# smscsim

![Run tests](https://github.com/ukarim/smscsim/workflows/run-tests/badge.svg)

Lightweight, zero-dependency and stupid SMSc simulator.

### Usage

1) Use prebuild docker image (from hub.docker.com)

```
docker run -p 2775:2775 -p 12775:12775 ukarim/smscsim
```

2) Build from sources (need golang compiler)

```
go build
./smscsim
```

then, just configure your smpp client to connect to `localhost:2775`

### Commands

Running the binary with no arguments starts the simulator, exactly as before.

```
./smscsim              # same as ./smscsim serve
./smscsim serve        # start the smpp and web servers
./smscsim tui          # start the smpp server with a terminal interface
./smscsim help         # show the available commands (also -h / --help)
```

`serve` accepts flags that override the environment variables
(flag > env variable > default):

```
./smscsim serve --smpp-port 2776 --web-port 12776 --failed-submits
```

### Terminal interface

`tui` starts the same smpp server inside a terminal interface, so sessions,
protocol activity and MO messages are handled without a browser. It does not
start the web server, so it has no `--web-port` flag:

```
./smscsim tui --smpp-port 2776 --failed-submits
```

Its flags follow the same precedence as the ones of `serve`
(flag > env variable > default):

* `--smpp-port` - port the smpp server listens on (env `SMSC_PORT`, default 2775)
* `--failed-submits` - make submit_sm requests fail (env `FAILED_SUBMITS`, default false)

The screen is split into three panes:

* **SESSIONS** - the bound smpp sessions, with their system id, bind type,
  remote address and bind time. The cursor selects the session an MO message is
  sent to.
* **EVENTS** - the protocol activity of the simulator as it happens: binds,
  unbinds, received and sent PDUs, and errors. The standard log is muted while
  the interface is running so it cannot overwrite the screen, and the oldest
  entries are dropped once the log is full.
* **MO MESSAGE** - sender, recipient and message text of a mobile originated
  message, delivered to the selected session with a _deliver_sm_ PDU.

`tab` and `shift+tab` move between the panes, `enter` sends the MO message from
the form, and `q` or `ctrl+c` quits. If the smpp port is already taken, the
command reports it and exits before the interface starts.

### Features

#### Delivery reports (DLR)

If it was requested by _submit_sm_ packet, delivery receipt will be returned
after 2 sec with a message state always set to _DELIVERED_.

#### MO messages

Mobile originated messages (from `smsc` to `smpp client`) can be sent using
special web page available at `http://localhost:12775` . MO message will be
delivered to the selected smpp session using a _deliver_sm_ PDU.

### Warning

* simulator implements only a small subset of the SMPP3.4 specification and supports only the following PDUs:
  - `bind_transmitter`, `bind_receiver`, `bind_transceiver`
  - `unbind`
  - `submit_sm`
  - `enquire_link`
  - `deliver_sm_resp`
* simulator does not perform PDU validation

### Env variables

* SMSC_PORT - override default smpp port
* WEB_PORT - override default web port
* FAILED_SUBMITS - if this is set to true, submit_sm requests will fail
  - for submit_sm with even sequence number smscsim will return submit_sm_resp with command_status set to 0x00000008 (System Error)
  - for submit_sm with odd sequence number smscsim will return DLR with UNDELIVERABLE message state
