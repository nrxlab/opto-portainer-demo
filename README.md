# Opto22 & Portainer demo using groov Manage REST API

Containerized collector for direct groov RIO I/O reads through the groov Manage REST API, with MQTT publishing to a minimal Mosquitto broker.

It polls the packed module endpoints:

```text
GET /manage/api/v1/io/local/modules/<module>/analog/values?channels=8
GET /manage/api/v1/io/local/modules/<module>/digital/values
```

It discovers channel configuration once at startup and publishes a compact name-keyed JSON payload.

# deployments

Clone this repository
```
git clone https://github.com/nrxlab/opto-portainer-demo.git
```

## configure

Copy the template and fill in your device's values:

```bash
cp .env.example .env
```

# edit .env: set OPTO_HOST and OPTO_API_KEY at a minimum

Supported output modes:

- `stdout`
- `mqtt`
- `both`

## build locally instead

If you want to build the collector image yourself (uses the `build:` section in `compose.yml`):

```bash
docker compose up --build
```

## discover actual module layout

```bash
docker compose run --rm opto-rio-rest-collector --discover
```

## run collector + broker

```bash
docker compose up -d
```

Follow the logs:

```bash
docker compose logs -f opto-rio-rest-collector
```

## subscribe to the broker

From another shell:

```bash
docker compose exec mosquitto mosquitto_sub -t 'opto/rio/#' -v
```

Using `mosquitto-clients` if installed:

```bash
mosquitto_sub -h localhost -p 1883 -t 'opto/rio/#' -v
```

## payload shape

```json
{
  "timestamp": "2026-09-03T14:15:32.804459972Z",
  "host": "10.0.2.55",
  "device": "local",
  "module": 0,
  "fields": {
    "di-button01": {"channel": 0, "kind": "digital", "value": true, "qualityError": false, "channelType": "0x50000079"},
    "ai-potentiometer": {"channel": 2, "kind": "analog", "value": 1.91, "unit": "V", "qualityError": false, "channelType": "0x60000018"},
    "ai-ambient-temp": {"channel": 3, "kind": "analog", "value": 25.9, "unit": "Degrees C", "qualityError": false, "channelType": "0x60000031"}
  }
}
```

Set `OPTO_INCLUDE_RAW=true` to also include raw packed analog/digital arrays.
