const direct = process.env.DIRECT_API_BASE_URL ?? "http://127.0.0.1:19529";
const proxy = process.env.PROXY_API_BASE_URL ?? "http://127.0.0.1:18529";
const password = process.env.SMOKE_PASSWORD ?? "LngBalance!2026";
const results = [];

function fail(message, payload) {
  const detail = payload === undefined ? "" : `\n${JSON.stringify(payload, null, 2)}`;
  throw new Error(message + detail);
}

async function api(label, base, path, options = {}) {
  const { method = "GET", token, body, status = 200, errorCode } = options;
  const headers = {
    "X-Request-ID": `gb529-smoke-${label.replace(/[^a-z0-9]/gi, "-").toLowerCase()}`,
  };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const response = await fetch(base + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const raw = await response.text();
  let payload;
  try {
    payload = raw ? JSON.parse(raw) : null;
  } catch {
    payload = raw;
  }
  if (response.status !== status) {
    fail(`${label}: expected HTTP ${status}, got ${response.status}`, payload);
  }
  if (errorCode && payload?.error?.code !== errorCode) {
    fail(`${label}: expected ${errorCode}, got ${payload?.error?.code}`, payload);
  }
  if (!response.headers.get("x-request-id") && !payload?.request_id) {
    fail(`${label}: response did not include a request ID`, payload);
  }
  results.push({ label, status: response.status, code: payload?.error?.code ?? "OK" });
  return payload;
}

async function login(email) {
  const response = await api(`${email} login`, proxy, "/api/v1/auth/login", {
    method: "POST",
    body: { email, password },
  });
  if (!response.data.token) fail(`${email}: login did not return a token`, response);
  return response.data.token;
}

async function runAndSubmit(analyst, tankID, start, end, label) {
  const run = await api(`${label} balance run`, proxy, "/api/v1/balances/run", {
    method: "POST",
    token: analyst,
    body: { tank_id: tankID, period_start: start, period_end: end },
    status: 201,
  });
  if (run.data.balance_status !== "calculating" || run.data.version !== 2) {
    fail(`${label}: calculation state or version is invalid`, run);
  }
  const submitted = await api(
    `${label} review submission`,
    proxy,
    `/api/v1/balances/${run.data.id}/submit`,
    { method: "POST", token: analyst, body: { version: run.data.version } },
  );
  if (submitted.data.balance_status !== "pending_review") {
    fail(`${label}: balance did not enter pending_review`, submitted);
  }
  return submitted.data;
}

async function main() {
  await api("direct health", direct, "/healthz");
  await api("proxied health", proxy, "/api/healthz");
  await api("proxied readiness", proxy, "/api/readyz");
  await api("unauthenticated tanks", proxy, "/api/v1/tanks", {
    status: 401,
    errorCode: "AUTH_REQUIRED",
  });

  const analyst = await login("analyst@lng.local");
  await api("analyst identity", proxy, "/api/v1/auth/me", { token: analyst });
  await api("tank list", proxy, "/api/v1/tanks", { token: analyst });
  await api("measurement list", proxy, "/api/v1/measurements", { token: analyst });
  await api("transfer list", proxy, "/api/v1/transfers", { token: analyst });

  const unique = String(Date.now()).slice(-9);
  const tankInput = {
    tank_code: `QA-${unique}`,
    name: `API verification tank ${unique}`,
    nominal_capacity_m3: 200000,
    min_level_m: 0,
    max_level_m: 20,
    reference_density_kgm3: 450,
    reference_temperature_c: -160,
    thermal_expansion_per_c: 0.0012,
    capacity_curve: [0, 10000],
    coefficient_version: "qa-v1",
    tank_status: "active",
  };
  const tank = await api("create tank", proxy, "/api/v1/tanks", {
    method: "POST",
    token: analyst,
    body: tankInput,
    status: 201,
  });
  const tankID = tank.data.id;
  const periodStart = "2026-07-01T01:00:00Z";
  const periodEnd = "2026-07-02T01:00:00Z";

  await api("missing boundary snapshots", proxy, "/api/v1/balances/run", {
    method: "POST",
    token: analyst,
    body: { tank_id: tankID, period_start: periodStart, period_end: periodEnd },
    status: 422,
    errorCode: "OPENING_SNAPSHOT_MISSING",
  });

  const measurement = {
    tank_id: tankID,
    liquid_temp_c: -160,
    vapor_pressure_kpa: 115,
    density_kgm3: 450,
    measurement_uncertainty_pct: 0.35,
    quality_flag: "good",
  };
  const opening = await api("create opening snapshot", proxy, "/api/v1/measurements", {
    method: "POST",
    token: analyst,
    status: 201,
    body: {
      ...measurement,
      measured_at: "2026-07-01T00:30:00Z",
      liquid_level_m: 10,
      source_note: "API smoke opening gauge snapshot",
    },
  });
  const closing = await api("create closing snapshot", proxy, "/api/v1/measurements", {
    method: "POST",
    token: analyst,
    status: 201,
    body: {
      ...measurement,
      measured_at: "2026-07-02T00:30:00Z",
      liquid_level_m: 9.96,
      source_note: "API smoke closing gauge snapshot",
    },
  });
  if (opening.data.id === closing.data.id) fail("boundary snapshots must be independent");

  const transfer = {
    tank_id: tankID,
    measurement_uncertainty_pct: 0.25,
    operation_status: "confirmed",
  };
  await api("create inflow", proxy, "/api/v1/transfers", {
    method: "POST",
    token: analyst,
    status: 201,
    body: {
      ...transfer,
      operation_type: "inflow",
      start_at: "2026-07-01T03:00:00Z",
      end_at: "2026-07-01T04:00:00Z",
      measured_mass_kg: 100000,
      counterparty_ref: "API-IN-001",
    },
  });
  await api("reject overlapping transfer", proxy, "/api/v1/transfers", {
    method: "POST",
    token: analyst,
    status: 409,
    errorCode: "TRANSFER_TIME_OVERLAP",
    body: {
      ...transfer,
      operation_type: "outflow",
      start_at: "2026-07-01T03:30:00Z",
      end_at: "2026-07-01T04:30:00Z",
      measured_mass_kg: 50000,
      counterparty_ref: "API-OVERLAP-001",
    },
  });
  await api("create outflow", proxy, "/api/v1/transfers", {
    method: "POST",
    token: analyst,
    status: 201,
    body: {
      ...transfer,
      operation_type: "outflow",
      start_at: "2026-07-01T05:00:00Z",
      end_at: "2026-07-01T06:00:00Z",
      measured_mass_kg: 65000,
      counterparty_ref: "API-OUT-001",
    },
  });
  await api("tank measurements", proxy, `/api/v1/measurements?tank_id=${tankID}`, {
    token: analyst,
  });
  await api("tank transfers", proxy, `/api/v1/transfers?tank_id=${tankID}`, {
    token: analyst,
  });
  await api("measurement quality", proxy, `/api/v1/tanks/${tankID}/measurement-quality`, {
    token: analyst,
  });

  const acceptedRun = await runAndSubmit(analyst, tankID, periodStart, periodEnd, "accepted-path");
  const uncertainty = await api(
    "uncertainty breakdown",
    proxy,
    `/api/v1/balances/${acceptedRun.id}/uncertainty`,
    { token: analyst },
  );
  if (uncertainty.data.balance_run_id !== acceptedRun.id || uncertainty.data.components.length !== 4) {
    fail("uncertainty evidence is incomplete", uncertainty);
  }

  const reviewer = await login("reviewer@lng.local");
  await api("reviewer mutation forbidden", proxy, "/api/v1/tanks", {
    method: "POST",
    token: reviewer,
    body: { ...tankInput, tank_code: `NO-${unique}` },
    status: 403,
    errorCode: "ACCESS_DENIED",
  });
  const accepted = await api(
    "accept review",
    proxy,
    `/api/v1/balances/${acceptedRun.id}/review`,
    {
      method: "POST",
      token: reviewer,
      body: {
        target_status: "accepted",
        version: acceptedRun.version,
        review_note: "Independent evidence review accepted.",
      },
    },
  );
  if (accepted.data.balance_status !== "accepted") fail("review was not accepted", accepted);

  const rejectedRun = await runAndSubmit(analyst, tankID, periodStart, periodEnd, "rejected-path");
  const rejected = await api(
    "reject review",
    proxy,
    `/api/v1/balances/${rejectedRun.id}/review`,
    {
      method: "POST",
      token: reviewer,
      body: {
        target_status: "rejected",
        version: rejectedRun.version,
        review_note: "Independent review requests source reconciliation.",
      },
    },
  );
  if (rejected.data.balance_status !== "rejected") fail("review was not rejected", rejected);

  const balances = await api("tank balances", proxy, `/api/v1/balances?tank_id=${tankID}`, {
    token: reviewer,
  });
  if (balances.meta.total < 2) fail("balance history is incomplete", balances);

  // 边界证据完整性闸门：晚录确认的期间内流入把待复核运行置为待重算。
  const gatedStart = "2026-07-02T01:00:00Z";
  const gatedEnd = "2026-07-03T01:00:00Z";
  await api("create gate-period closing snapshot", proxy, "/api/v1/measurements", {
    method: "POST",
    token: analyst,
    status: 201,
    body: {
      ...measurement,
      measured_at: "2026-07-03T00:30:00Z",
      liquid_level_m: 9.9,
      source_note: "API smoke gate period closing snapshot",
    },
  });
  const gatedSubmitted = await runAndSubmit(analyst, tankID, gatedStart, gatedEnd, "gate-path");
  await api("create late confirmed inflow", proxy, "/api/v1/transfers", {
    method: "POST",
    token: analyst,
    status: 201,
    body: {
      tank_id: tankID,
      operation_type: "inflow",
      start_at: "2026-07-02T08:00:00Z",
      end_at: "2026-07-02T09:00:00Z",
      measured_mass_kg: 32000,
      measurement_uncertainty_pct: 0.24,
      counterparty_ref: "API-LATE-IN-002",
      operation_status: "confirmed",
    },
  });
  const gated = await api("gated balance readback", proxy, `/api/v1/balances/${gatedSubmitted.id}`, {
    token: reviewer,
  });
  if (gated.data.balance_status !== "recalculate_required") {
    fail("late confirmed transfer must flag the run as recalculate_required", gated);
  }
  if (!Array.isArray(gated.data.recalculation_reason_json) || gated.data.recalculation_reason_json[0]?.code !== "LATE_TRANSFER_CONFIRMED") {
    fail("recalculation reason must be persisted and readable after refresh", gated);
  }
  await api("accept gated run forbidden", proxy, `/api/v1/balances/${gatedSubmitted.id}/review`, {
    method: "POST",
    token: reviewer,
    status: 409,
    errorCode: "BALANCE_RECALCULATE_REQUIRED",
    body: {
      target_status: "accepted",
      version: gated.data.version,
      review_note: "Reviewer attempt while gate is closed must be rejected.",
    },
  });
  const replacement = await api("reviewer atomic recalculation", proxy, `/api/v1/balances/${gatedSubmitted.id}/recalculate`, {
    method: "POST",
    token: reviewer,
    body: { version: gated.data.version },
  });
  if (
    replacement.data.superseded.balance_status !== "superseded" ||
    replacement.data.superseded.superseded_by_id !== replacement.data.recalculated.id ||
    replacement.data.recalculated.balance_status !== "pending_review" ||
    replacement.data.recalculated.supersedes_id !== replacement.data.superseded.id
  ) {
    fail("recalculation replacement chain is inconsistent", replacement);
  }
  await api("duplicate recalculation rejected", proxy, `/api/v1/balances/${gatedSubmitted.id}/recalculate`, {
    method: "POST",
    token: reviewer,
    status: 409,
    errorCode: "BALANCE_ALREADY_SUPERSEDED",
    body: { version: replacement.data.superseded.version },
  });
  await api(
    "accept recalculated run",
    proxy,
    `/api/v1/balances/${replacement.data.recalculated.id}/review`,
    {
      method: "POST",
      token: reviewer,
      body: {
        target_status: "accepted",
        version: replacement.data.recalculated.version,
        review_note: "Recalculated with the late inflow evidence and now accepted.",
      },
    },
  );
  const refreshChain = await api(
    "replacement chain readback",
    proxy,
    `/api/v1/balances/${gatedSubmitted.id}`,
    { token: reviewer },
  );
  if (refreshChain.data.balance_status !== "superseded" || refreshChain.data.superseded_by_id !== replacement.data.recalculated.id) {
    fail("replacement chain must read back consistently after refresh", refreshChain);
  }

  const audits = await api(
    "audit query",
    proxy,
    "/api/v1/audits?page=1&page_size=100&entity_type=balance_run",
    { token: reviewer },
  );
  if (audits.meta.total < 10) fail("balance audit trail is unexpectedly short", audits);
  const auditActions = audits.data.map((event) => event.action);
  for (const expected of ["balance_run.recalculate_required", "balance_run.recalculated", "balance_run.superseded"]) {
    if (!auditActions.includes(expected)) fail(`audit trail missing ${expected}`, audits);
  }

  console.log("API_SMOKE_PASS");
  console.table(results);
  console.log(JSON.stringify({
    tank_id: tankID,
    tank_code: tank.data.tank_code,
    accepted_balance_id: acceptedRun.id,
    rejected_balance_id: rejectedRun.id,
    audit_total: audits.meta.total,
  }, null, 2));
}

main().catch((error) => {
  console.error("API_SMOKE_FAIL");
  console.error(error.stack ?? error);
  process.exit(1);
});
