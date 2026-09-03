# Connectors

Connectors turn configured external capabilities into schema-described tools.
Inputs are validated before execution, secret values can use `secret:ENV_VAR`,
and result mappings keep only the fields that an agent needs as evidence.

## REST / HTTP

Use `type: "http"` for REST-style endpoints. A capability declares a path,
HTTP method, input schema, and optional JSON body template. Only a configured
GET or HEAD capability without a request body can run autonomously, and only
when the request enables external search. Other methods require an explicit,
user-initiated execution.

## JSON-RPC 2.0

Use `type: "rpc"` with the endpoint in `base_url`. A capability's
`rpc_method` is sent as a JSON-RPC 2.0 `method`; its validated input object is
sent as `params` without text templating. The tool result is the response's
`result` value, so output mappings address fields within that value.

```json
{
  "id": "catalog-rpc",
  "name": "Product catalog",
  "type": "rpc",
  "base_url": "https://catalog.example/api/rpc",
  "enabled": true,
  "capabilities": [{
    "name": "catalog_lookup",
    "description": "Looks up a product by SKU",
    "type": "tool",
    "rpc_method": "catalog.lookup",
    "read_only": true,
    "input_schema": {
      "type": "object",
      "properties": {"sku": {"type": "string"}},
      "required": ["sku"]
    },
    "output_map": [{"field": "name", "as": "name"}]
  }]
}
```

JSON-RPC uses POST, which does not imply safety. A capability is autonomous
only when its operator explicitly sets `read_only: true`; all others remain
manual.

## SQL

Use `type: "sql"` for parameterized, configured SQLite, MariaDB/MySQL, or
MSSQL queries. Capability queries must be SELECT-only. Row limits are enforced
when configured, and autonomous use is limited to the strict read-only subset.
