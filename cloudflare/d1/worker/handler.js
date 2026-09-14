/**
 * The Worker half of the drops D1 wire protocol.
 *
 * This is a dependency-free ES module you MOUNT inside your own
 * Worker, not one you deploy as-is. It knows the wire format and
 * nothing else: no authentication, no routing, no logging, no tenant
 * resolution. Those stay yours, which is the point — the only thing
 * that has to come from drops is the format, because that is the part
 * whose drift returns wrong rows instead of failing to build.
 *
 *   import { handleD1Request } from "./drops-d1-handler.js";
 *
 *   export default {
 *     async fetch(request, env) {
 *       if (new URL(request.url).pathname !== "/d1") {
 *         return new Response("not found", { status: 404 });
 *       }
 *       if (!authorized(request, env)) {
 *         return new Response("forbidden", { status: 403 });
 *       }
 *       return handleD1Request(request, env.DB);
 *     },
 *   };
 *
 * The Go side is github.com/bernardoforcillo/drops/cloudflare/d1,
 * whose protocol.go is the normative description of every field.
 * fixtures.json is the conformance suite; both sides run it, so a
 * change to one half that the other has not made shows up as a test
 * failure rather than as a production incident.
 */

/** The protocol version this handler speaks. */
export const PROTOCOL_VERSION = 1;

/**
 * Handles one protocol request against a D1 binding.
 *
 * Returns HTTP 200 for anything that reached D1 — including a
 * statement SQLite refused, which comes back in the body's `error`.
 * The split is deliberate: a SQL failure is the caller's to fix and
 * must never be retried, while a non-2xx may be worth another
 * attempt, and the Go client treats them that way.
 *
 * @param {Request} request
 * @param {D1Database} db
 * @returns {Promise<Response>}
 */
export async function handleD1Request(request, db) {
  if (request.method !== "POST") {
    return protocolError("method must be POST", 405);
  }
  if (!db) {
    return protocolError("no D1 binding was passed to handleD1Request", 500);
  }

  let body;
  try {
    body = await request.json();
  } catch (e) {
    return protocolError(`request body is not JSON: ${e.message}`, 400);
  }

  const problem = validate(body);
  if (problem) {
    return protocolError(problem, 400);
  }

  const prepared = body.statements.map((s) => {
    const stmt = db.prepare(s.sql);
    const params = s.params ?? [];
    return params.length > 0 ? stmt.bind(...params.map(decodeParam)) : stmt;
  });

  try {
    // One statement runs on its own; several run as a D1 batch,
    // which is the implicit transaction D1 offers in place of
    // BEGIN/COMMIT. The Go client relies on that: its Tx buffers
    // statements precisely so they arrive here as one list.
    const results =
      prepared.length === 1
        ? [await runOne(prepared[0])]
        : (await db.batch(prepared)).map(fromBatchResult);

    return json({ protocol: PROTOCOL_VERSION, results }, 200);
  } catch (e) {
    // D1 reports a SQLite failure by throwing. The message is
    // SQLite's own text, and passing it through unaltered is what
    // lets the Go side classify it: it is the only classification on
    // the wire.
    return json(
      {
        protocol: PROTOCOL_VERSION,
        error: {
          message: sqliteMessage(e),
          code: e?.cause?.code ?? e?.code ?? "",
          index: -1,
        },
      },
      200,
    );
  }
}

/**
 * Runs a single prepared statement and shapes it as a protocol
 * result.
 *
 * `raw({ columnNames: true })` is what gives an ordered column list
 * and positional rows. `all()` would return objects, and an object
 * has no order: a positional Scan on the Go side would have to guess
 * which key a destination meant, and a join projecting two columns of
 * the same name would lose one of them.
 */
async function runOne(stmt) {
  const out = await stmt.raw({ columnNames: true });
  // With columnNames, the first element is the header row.
  const columns = out.length > 0 ? out[0] : [];
  const rows = out.length > 1 ? out.slice(1) : [];
  return { columns, rows, meta: normalizeMeta(stmt.meta ?? {}) };
}

/**
 * Shapes one element of a db.batch() result.
 *
 * batch() answers with `results` as objects rather than raw rows, so
 * the column order has to be recovered from the first row's key
 * order — which is the order SQLite projected them in. It is the one
 * place this protocol cannot get order from the engine directly, and
 * the reason a single statement takes the raw() path above instead.
 */
function fromBatchResult(r) {
  const rows = r?.results ?? [];
  if (rows.length === 0) {
    return { columns: [], rows: [], meta: normalizeMeta(r?.meta ?? {}) };
  }
  const columns = Object.keys(rows[0]);
  return {
    columns,
    rows: rows.map((row) => columns.map((c) => row[c])),
    meta: normalizeMeta(r?.meta ?? {}),
  };
}

/**
 * Fills in the meta fields the Go side reads, so a runtime that omits
 * one does not decode as a missing key.
 */
function normalizeMeta(m) {
  return {
    served_by: m.served_by ?? "",
    served_by_region: m.served_by_region ?? "",
    served_by_primary: m.served_by_primary ?? false,
    duration: m.duration ?? 0,
    changes: m.changes ?? 0,
    last_row_id: m.last_row_id ?? 0,
    changed_db: m.changed_db ?? false,
    size_after: m.size_after ?? 0,
    rows_read: m.rows_read ?? 0,
    rows_written: m.rows_written ?? 0,
  };
}

/**
 * Turns a protocol parameter into what D1's bind() takes.
 *
 * Only one shape needs converting: a BLOB travels as an array of byte
 * values, because JSON has no binary, and D1 wants an ArrayBuffer.
 * Everything else — null, number, string — binds as it arrives.
 */
function decodeParam(p) {
  if (Array.isArray(p)) {
    return new Uint8Array(p).buffer;
  }
  return p;
}

/** Validates the request envelope, returning a problem or null. */
function validate(body) {
  if (body === null || typeof body !== "object") {
    return "request body must be a JSON object";
  }
  if (body.protocol !== PROTOCOL_VERSION) {
    return `client speaks protocol ${body.protocol}, this handler speaks ${PROTOCOL_VERSION}`;
  }
  if (!Array.isArray(body.statements) || body.statements.length === 0) {
    return "statements must be a non-empty array";
  }
  for (const [i, s] of body.statements.entries()) {
    if (typeof s?.sql !== "string" || s.sql === "") {
      return `statement ${i} has no sql`;
    }
    if (s.params !== undefined && !Array.isArray(s.params)) {
      return `statement ${i} has params that are not an array`;
    }
  }
  return null;
}

/**
 * Extracts SQLite's own message from whatever D1 threw.
 *
 * D1 wraps the engine's text in its own ("D1_ERROR: UNIQUE constraint
 * failed: users.email"), and the Go side matches on the engine's
 * wording, so the wrapper is stripped and the text is otherwise left
 * exactly as it came.
 */
function sqliteMessage(e) {
  const raw = e?.cause?.message ?? e?.message ?? String(e);
  return raw.replace(/^D1_ERROR:\s*/, "").trim();
}

function json(body, status) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function protocolError(message, status) {
  return json(
    { protocol: PROTOCOL_VERSION, error: { message, code: "DROPS_PROTOCOL", index: -1 } },
    status,
  );
}
