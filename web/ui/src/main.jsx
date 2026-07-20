import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";

const statusLabels = {
  available: "Available",
  reserved: "Reserved",
  claimed: "Claimed",
};

const calendarHourHeight = 28;
const minCalendarHours = 10;
const scheduleLaneGap = 2;
const hourMs = 60 * 60 * 1000;
const dayMs = 24 * hourMs;
const historyPageSize = 50;

let historyFilterID = 0;

function App() {
  const [auth, setAuth] = useState({ checking: true, authenticated: false, user: "", role: "" });
  const [registrationEnabled, setRegistrationEnabled] = useState(false);
  const [servers, setServers] = useState([]);
  const [fleet, setFleet] = useState([]);
  const [users, setUsers] = useState([]);
  const [selectedServerId, setSelectedServerId] = useState("");
  const [selectedGPUs, setSelectedGPUs] = useState(new Set());
  const [activeGPU, setActiveGPU] = useState(null);
  const [view, setView] = useState("gpu");
  const [search, setSearch] = useState("");
  const [addOpen, setAddOpen] = useState(false);
  const [claimOpen, setClaimOpen] = useState(false);
  const [passwordOpen, setPasswordOpen] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [userOpen, setUserOpen] = useState(false);
  const [deleteUserTarget, setDeleteUserTarget] = useState(null);
  const [allowTarget, setAllowTarget] = useState(null);
  const [revokeTarget, setRevokeTarget] = useState(null);
  const [scheduleTarget, setScheduleTarget] = useState(null);
  const [reserveHint, setReserveHint] = useState(null);
  const [reservationSuccess, setReservationSuccess] = useState(null);
  const [successKey, setSuccessKey] = useState("");
  const [error, setError] = useState("");
  const [loginError, setLoginError] = useState("");
  const [historySummary, setHistorySummary] = useState(null);
  const [historySessions, setHistorySessions] = useState([]);
  const [historyTarget, setHistoryTarget] = useState(null);
  const [historyJobs, setHistoryJobs] = useState([]);
  const [historyLoading, setHistoryLoading] = useState(false);
  const [historyLoadingMore, setHistoryLoadingMore] = useState(false);
  const [historyNextCursor, setHistoryNextCursor] = useState("");
  const [historyFilters, setHistoryFilters] = useState({ groups: [] });
  const settingsRef = useRef(null);
  const historyRequestRef = useRef(0);

  useEffect(() => {
    checkSession();
  }, []);

  useEffect(() => {
    if (!auth.authenticated) {
      return undefined;
    }

    let stopped = false;
    let running = false;
    let timer;
    let controller;

    function clearTimer() {
      if (timer !== undefined) {
        window.clearTimeout(timer);
        timer = undefined;
      }
    }

    function schedule() {
      clearTimer();
      if (stopped || document.hidden) {
        return;
      }
      timer = window.setTimeout(() => {
        timer = undefined;
        void poll();
      }, 5000);
    }

    async function poll() {
      if (stopped || running || document.hidden) {
        return;
      }
      running = true;
      controller = new AbortController();
      try {
        await refresh({ signal: controller.signal, isCurrent: () => !stopped });
      } finally {
        running = false;
        controller = undefined;
        schedule();
      }
    }

    function handleVisibilityChange() {
      if (document.hidden) {
        clearTimer();
        return;
      }
      if (!running) {
        clearTimer();
        void poll();
      }
    }

    document.addEventListener("visibilitychange", handleVisibilityChange);
    void poll();
    return () => {
      stopped = true;
      clearTimer();
      controller?.abort();
      document.removeEventListener("visibilitychange", handleVisibilityChange);
    };
  }, [auth.authenticated]);

  useEffect(() => {
    if (auth.authenticated && auth.role === "admin" && view === "users") {
      loadUsers();
    }
    if (auth.authenticated && auth.role !== "admin" && view === "users") {
      setView("gpu");
    }
  }, [auth.authenticated, auth.role, view]);

  useEffect(() => {
    if (!auth.authenticated || view !== "history") {
      return undefined;
    }
    historyRequestRef.current += 1;
    if (historyFilterErrors(historyFilters).length > 0) {
      return undefined;
    }
    const controller = new AbortController();
    const timer = window.setTimeout(() => {
      void loadHistory({ filters: historyFilters, signal: controller.signal });
    }, 400);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [auth.authenticated, view, historyFilters]);

  useEffect(() => {
    if (!settingsOpen) {
      return undefined;
    }

    function closeSettings(event) {
      if (!settingsRef.current?.contains(event.target)) {
        setSettingsOpen(false);
      }
    }

    function closeSettingsOnEscape(event) {
      if (event.key === "Escape") {
        setSettingsOpen(false);
      }
    }

    document.addEventListener("pointerdown", closeSettings);
    document.addEventListener("keydown", closeSettingsOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeSettings);
      document.removeEventListener("keydown", closeSettingsOnEscape);
    };
  }, [settingsOpen]);

  async function checkSession() {
    try {
      const session = await api("/api/session");
      setAuth({
        checking: false,
        authenticated: Boolean(session.authenticated),
        user: session.user || "",
        role: session.role || "",
      });
      setRegistrationEnabled(Boolean(session.registration_enabled));
    } catch {
      setAuth({ checking: false, authenticated: false, user: "", role: "" });
      setRegistrationEnabled(false);
    }
  }

  async function login(values) {
    try {
      const session = await api("/api/login", {
        method: "POST",
        body: JSON.stringify({ username: values.username, password: values.password }),
      });
      setLoginError("");
      setError("");
      setAuth({
        checking: false,
        authenticated: true,
        user: session.user || values.username,
        role: session.role || "user",
      });
    } catch (err) {
      setLoginError(err.message);
    }
  }

  async function register(values) {
    try {
      const session = await api("/api/register", {
        method: "POST",
        body: JSON.stringify({ username: values.username, password: values.password }),
      });
      setLoginError("");
      setError("");
      setAuth({
        checking: false,
        authenticated: true,
        user: session.user || values.username,
        role: session.role || "user",
      });
    } catch (err) {
      setLoginError(err.message);
    }
  }

  async function logout() {
    try {
      await api("/api/logout", { method: "POST" });
    } finally {
      setAuth({ checking: false, authenticated: false, user: "", role: "" });
      setServers([]);
      setFleet([]);
      setUsers([]);
      setSelectedServerId("");
      setSelectedGPUs(new Set());
      setActiveGPU(null);
      setView("gpu");
      setPasswordOpen(false);
      setSettingsOpen(false);
      setDeleteUserTarget(null);
      setReservationSuccess(null);
      setSuccessKey("");
      setHistorySummary(null);
      setHistorySessions([]);
      setHistoryTarget(null);
      setHistoryJobs([]);
      setHistoryNextCursor("");
      setHistoryFilters({ groups: [] });
      historyRequestRef.current += 1;
    }
  }

  async function changePassword(values) {
    await api("/api/password", {
      method: "POST",
      body: JSON.stringify(values),
    });
  }

  async function loadUsers() {
    try {
      const list = await api("/api/users");
      setUsers(list);
    } catch (err) {
      setError(err.message);
    }
  }

  async function loadHistory({ filters = historyFilters, cursor = "", append = false, signal } = {}) {
    if (historyFilterErrors(filters).length > 0) {
      return;
    }
    const requestID = ++historyRequestRef.current;
    if (append) {
      setHistoryLoadingMore(true);
    } else {
      setHistoryLoading(true);
    }
    try {
      const response = await api("/api/history/search", {
        method: "POST",
        signal,
        body: JSON.stringify({
          filter: historySearchExpression(filters),
          limit: historyPageSize,
          cursor,
        }),
      });
      if (signal?.aborted || requestID !== historyRequestRef.current) {
        return;
      }
      setHistorySummary(response.summary || null);
      setHistorySessions((current) => append ? [...current, ...(response.sessions || [])] : (response.sessions || []));
      setHistoryNextCursor(response.next_cursor || "");
    } catch (err) {
      if (err.name !== "AbortError" && requestID === historyRequestRef.current) {
        setError(err.message);
      }
    } finally {
      if (requestID === historyRequestRef.current) {
        setHistoryLoading(false);
        setHistoryLoadingMore(false);
      }
    }
  }

  async function openHistorySession(id) {
    try {
      const [session, response] = await Promise.all([
        api(`/api/history/sessions/${id}`),
        api(`/api/history/sessions/${id}/jobs`),
      ]);
      setHistoryTarget(session);
      setHistoryJobs(response.jobs || []);
    } catch (err) {
      setError(err.message);
    }
  }

  async function saveHistoryResult(values) {
    if (!historyTarget) {
      return;
    }
    const result = await api(`/api/history/sessions/${historyTarget.id}/result`, {
      method: "PUT",
      body: JSON.stringify(values),
    });
    setHistoryTarget((current) => current ? { ...current, result } : current);
    await loadHistory();
  }

  async function refresh({ signal, isCurrent } = {}) {
    try {
      const snapshot = await api("/api/fleet/snapshot", { signal });
      if (signal?.aborted || (isCurrent && !isCurrent())) {
        return;
      }
      const nextFleet = snapshot.servers || [];
      const serverList = nextFleet.map((item) => item.server).filter(Boolean);
      setServers(serverList);
      setFleet(nextFleet);
      setSelectedServerId((currentId) => {
        if (currentId && serverList.some((server) => server.id === currentId)) {
          return currentId;
        }
        return serverList[0]?.id || nextFleet[0]?.server?.id || "";
      });
    } catch (err) {
      if (signal?.aborted || err.name === "AbortError" || (isCurrent && !isCurrent())) {
        return;
      }
      if (err.status === 401) {
        setAuth({ checking: false, authenticated: false, user: "", role: "" });
        return;
      }
      setError(err.message);
    }
  }

  const current = useMemo(() => {
    return fleet.find((item) => item.server.id === selectedServerId) || fleet[0] || null;
  }, [fleet, selectedServerId]);

  const currentServerId = current?.server?.id || selectedServerId;
  const gpus = current?.snapshot?.gpus || [];
  const tokens = current?.snapshot?.tokens || [];
  const reservations = current?.snapshot?.reservations || [];
  const authorizations = current?.snapshot?.authorizations || [];
  const isAdmin = auth.role === "admin";
  const scheduleToken = scheduleTarget
    ? tokens.find((token) => token.id === scheduleTarget.id && token.mode === "reserved")
    : null;
  const selectedGPUList = Array.from(selectedGPUs).sort((a, b) => a - b);
  const allGPUIds = gpus.map((gpu) => gpu.id);
  const availableGPUIds = gpus
    .filter((gpu) => (gpu.state || "available") === "available")
    .map((gpu) => gpu.id);
  const allAvailableSelected =
    availableGPUIds.length > 0 && availableGPUIds.every((gpuId) => selectedGPUs.has(gpuId));
  const displayGPU = activeGPU ?? selectedGPUList[0] ?? gpus[0]?.id ?? 0;

  function selectServer(id) {
    setSelectedServerId(id);
    setSelectedGPUs(new Set());
    setActiveGPU(null);
    setReservationSuccess(null);
    setSuccessKey("");
  }

  function toggleGPU(id) {
    setActiveGPU(id);
    setSelectedGPUs((previous) => {
      const next = new Set(previous);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  }

  function selectAllGPUs() {
    if (allAvailableSelected) {
      setSelectedGPUs(new Set());
      setActiveGPU(null);
      return;
    }
    setSelectedGPUs(new Set(availableGPUIds));
    if (availableGPUIds.length > 0) {
      setActiveGPU(availableGPUIds[0]);
    }
  }

  function showSelectGPUHint() {
    setReserveHint({
      title: "Select GPU first",
      message: "Choose one or more GPUs from the grid before submitting a reservation.",
    });
  }

  function showReservationDetailsHint() {
    setReserveHint({
      title: "Complete reservation",
      message: "Fill Purpose, Start, and End before submitting a reservation.",
    });
  }

  function showReservationConflictHint(conflict) {
    setReserveHint({
      title: "Schedule conflict",
      message: `GPU ${conflict.gpus.join(", ")} already has a reservation from ${timeLabel(conflict.start)} to ${timeLabel(conflict.end)}.`,
    });
  }

  function showBusyGPUHint(conflict) {
    const gpuLabel = conflict.gpus.join(", ");
    const multiple = conflict.gpus.length > 1;
    setReserveHint({
      title: "Reservation unavailable",
      message: `GPU ${gpuLabel} ${multiple ? "are" : "is"} busy. ${multiple ? "Processes are" : "A process is"} already running on ${multiple ? "these GPUs" : "this GPU"}. Stop ${multiple ? "them" : "it"} or choose another GPU/time window.`,
    });
  }

  async function addServer(values) {
    await api("/api/servers", {
      method: "POST",
      body: JSON.stringify(values),
    });
    setAddOpen(false);
    await refresh();
  }

  async function reserve(values) {
    if (selectedGPUList.length === 0) {
      showSelectGPUHint();
      return;
    }
    if (!reservationDetailsComplete(values)) {
      showReservationDetailsHint();
      return;
    }
    const targets = selectedGPUList;
    try {
      const result = await api(`/api/servers/${currentServerId}/reservations`, {
        method: "POST",
        body: JSON.stringify({
          name: auth.user,
          purpose: values.purpose,
          gpus: targets,
          starts_at: new Date(values.start).toISOString(),
          expires_at: new Date(values.end).toISOString(),
        }),
      });
      setReservationSuccess({
        id: result.token_id,
        token: {
          id: result.token_id,
          name: auth.user,
          mode: result.mode || "reserved",
        },
        label: values.purpose || "Reservation",
        holder: auth.user,
        purpose: values.purpose,
        gpus: result.gpus?.length ? result.gpus : targets,
        start: new Date(result.starts_at || values.start),
        end: new Date(result.expires_at || values.end),
      });
      setSelectedGPUs(new Set());
      await refresh();
    } catch (err) {
      setReserveHint({
        title: "Reservation unavailable",
        message: formatReservationError(err.message),
      });
    }
  }

  async function createClaimKey() {
    const result = await api(`/api/servers/${currentServerId}/claim-keys`, {
      method: "POST",
      body: JSON.stringify({ name: auth.user }),
    });
    setClaimOpen(false);
    setSuccessKey(result.token || "");
    await refresh();
  }

  async function showKey(tokenId) {
    try {
      const status = await api(`/api/servers/${currentServerId}/show-key`, {
        method: "POST",
        body: JSON.stringify({ id: tokenId }),
      });
      const token = (status.tokens || []).find((item) => item.id === tokenId);
      setSuccessKey(token?.key || "");
      return Boolean(token?.key);
    } catch (err) {
      setError(err.message);
      return false;
    }
  }

  async function allowKey(values) {
    if (!allowTarget) {
      return;
    }
    const result = await api(`/api/servers/${currentServerId}/allow`, {
      method: "POST",
      body: JSON.stringify({ id: allowTarget.id, ...values }),
    });
    void refresh();
    return {
      ...result,
      id: result.authorization_id,
      token_id: allowTarget.id,
      mode: result.mode || values.mode,
      container_pattern: result.container_pattern || values.container,
      namespace: result.namespace || values.namespace,
      username: result.username || values.user,
    };
  }

  async function revokeRule(ruleId) {
    await api(`/api/servers/${currentServerId}/revoke`, {
      method: "POST",
      body: JSON.stringify({ id: ruleId }),
    });
    void refresh();
  }

  async function createUser(values) {
    const user = await api("/api/users", {
      method: "POST",
      body: JSON.stringify(values),
    });
    setUserOpen(false);
    setUsers((previous) => [...previous, user]);
  }

  async function deleteUser(username) {
    await api("/api/users", {
      method: "DELETE",
      body: JSON.stringify({ username }),
    });
    setUsers((previous) => previous.filter((user) => !sameText(user.username, username)));
  }

  async function revokeTargetItem(target) {
    try {
      await api(`/api/servers/${currentServerId}/revoke`, {
        method: "POST",
        body: JSON.stringify({ id: target.id }),
      });
      setError("");
      setRevokeTarget(null);
      setScheduleTarget(null);
      setReservationSuccess(null);
      await refresh();
    } catch (err) {
      setError(err.message);
      throw err;
    }
  }

  const visibleServers = servers.filter((server) => {
    const query = search.trim().toLowerCase();
    return !query || `${server.name} ${server.endpoint}`.toLowerCase().includes(query);
  });

  if (auth.checking) {
    return <LoadingScreen />;
  }

  if (!auth.authenticated) {
    return (
      <LoginScreen
        error={loginError}
        registrationEnabled={registrationEnabled}
        onLogin={login}
        onRegister={register}
        onResetError={() => setLoginError("")}
      />
    );
  }

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="brand-row">
          <BrandLockup className="brand" showMark={false} />
          <div className="settings-menu" ref={settingsRef}>
            <button
              type="button"
              className="settings-button"
              aria-label="Settings menu"
              aria-expanded={settingsOpen}
              aria-haspopup="menu"
              title="Settings menu"
              onClick={() => setSettingsOpen((open) => !open)}
            >
              <MenuIcon />
            </button>
            {settingsOpen && (
              <div className="settings-popover" role="menu">
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    setSettingsOpen(false);
                    setPasswordOpen(true);
                  }}
                >
                  Change password
                </button>
                <button type="button" role="menuitem" onClick={logout}>
                  Log out
                </button>
              </div>
            )}
          </div>
        </div>
        <input
          className="sidebar-search"
          placeholder="Search nodes..."
          value={search}
          onChange={(event) => setSearch(event.target.value)}
        />
        <div className="nav-title">Nodes</div>
        <div className="server-list">
          {visibleServers.map((server) => {
            const item = fleet.find((entry) => entry.server.id === server.id);
            return (
              <button
                key={server.id}
                className={`server-row ${server.id === currentServerId ? "active" : ""}`}
                onClick={() => selectServer(server.id)}
              >
                <span className={`server-dot ${item?.online ? "online" : "offline"}`} />
                <span className="server-name">{server.name}</span>
              </button>
            );
          })}
        </div>
        {isAdmin && (
          <button className="sidebar-add" onClick={() => setAddOpen(true)}>
            Add server
          </button>
        )}
      </aside>

      <main className="workspace">
        <header className="topbar">
          <div>
            <p className="eyebrow">{view === "history" ? "History" : "Nodes"}</p>
            <h1>{view === "history" ? "Reservation dashboard" : current?.server?.name || "No server selected"}</h1>
            {view !== "history" && !current?.server && <p className="muted">Add a RocGuard node to begin.</p>}
          </div>
          <div className="topbar-actions">
            <button className={view === "gpu" ? "tab active" : "tab"} onClick={() => setView("gpu")}>
              Schedule
            </button>
            <button className={view === "keys" ? "tab active" : "tab"} onClick={() => setView("keys")}>
              Key
            </button>
            <button className={view === "history" ? "tab active" : "tab"} onClick={() => setView("history")}>
              Dashboard
            </button>
            {isAdmin && (
              <button className={view === "users" ? "tab active" : "tab"} onClick={() => setView("users")}>
                Users
              </button>
            )}
          </div>
        </header>

        {error && <div className="banner" role="alert">{error}</div>}
        {current?.error && <div className="banner" role="alert">{current.error}</div>}

        {view === "gpu" ? (
          <div className="dashboard-grid">
            <section className="content-panel">
              <div className="section-heading">
                <div>
                  <h2>Available GPUs</h2>
                  <p className="muted">Select one or more GPUs, then reserve a schedule window.</p>
                </div>
                <div className="section-tools">
                  <Legend />
                  <button
                    type="button"
                    className={`select-all-button ${allAvailableSelected ? "active" : ""}`}
                    aria-pressed={allAvailableSelected}
                    onClick={selectAllGPUs}
                  >
                    <span className="select-all-box">{allAvailableSelected ? "✓" : ""}</span>
                    Select All
                  </button>
                </div>
              </div>
              <div className="gpu-grid">
                {gpus.map((gpu) => (
                  <GPUCard
                    key={gpu.id}
                    gpu={gpu}
                    selected={selectedGPUs.has(gpu.id)}
                    onClick={() => toggleGPU(gpu.id)}
                  />
                ))}
              </div>
            </section>
            <aside className="inspector">
              <Schedule
                gpu={displayGPU}
                allGPUs={allGPUIds}
                selected={selectedGPUList}
                reservations={reservations}
                onOpen={setScheduleTarget}
              />
              <ReserveForm
                owner={auth.user}
                selected={selectedGPUList}
                gpus={gpus}
                reservations={reservations}
                onMissingSelection={showSelectGPUHint}
                onMissingDetails={showReservationDetailsHint}
                onConflict={showReservationConflictHint}
                onBusy={showBusyGPUHint}
                onSubmit={reserve}
              />
            </aside>
          </div>
        ) : view === "keys" ? (
          <KeysView
            tokens={tokens}
            reservations={reservations}
            canCreate={isAdmin}
            onCreate={() => setClaimOpen(true)}
            onAllow={setAllowTarget}
            onShow={showKey}
            onRevoke={(token) => setRevokeTarget({ ...token, kind: "key" })}
          />
        ) : view === "history" ? (
          <HistoryDashboard
            summary={historySummary}
            sessions={historySessions}
            servers={servers}
            filters={historyFilters}
            loading={historyLoading}
            loadingMore={historyLoadingMore}
            nextCursor={historyNextCursor}
            onFilters={setHistoryFilters}
            onLoadMore={() => loadHistory({ cursor: historyNextCursor, append: true })}
            onOpen={openHistorySession}
          />
        ) : (
          <UsersView
            users={users}
            currentUser={auth.user}
            onCreate={() => setUserOpen(true)}
            onDelete={setDeleteUserTarget}
          />
        )}
      </main>

      {isAdmin && addOpen && <AddServerModal onClose={() => setAddOpen(false)} onSubmit={addServer} />}
      {isAdmin && claimOpen && <ClaimKeyModal owner={auth.user} onClose={() => setClaimOpen(false)} onSubmit={createClaimKey} />}
      {allowTarget && (
        <AllowKeyModal
          token={allowTarget}
          rules={authorizations.filter((authorization) => authorization.token_id === allowTarget.id)}
          onClose={() => setAllowTarget(null)}
          onSubmit={allowKey}
          onRemove={revokeRule}
        />
      )}
      {passwordOpen && <ChangePasswordModal onClose={() => setPasswordOpen(false)} onSubmit={changePassword} />}
      {isAdmin && userOpen && <CreateUserModal onClose={() => setUserOpen(false)} onSubmit={createUser} />}
      {isAdmin && deleteUserTarget && (
        <DeleteUserModal
          user={deleteUserTarget}
          onClose={() => setDeleteUserTarget(null)}
          onSubmit={() => deleteUser(deleteUserTarget.username)}
        />
      )}
      {revokeTarget && (
        <RevokeModal
          target={revokeTarget}
          onClose={() => setRevokeTarget(null)}
          onSubmit={() => revokeTargetItem(revokeTarget)}
        />
      )}
      {scheduleTarget && !revokeTarget && (
        <ScheduleDetailModal
          target={scheduleTarget}
          canAuthorize={Boolean(scheduleToken) && (isAdmin || sameText(scheduleTarget.holder, auth.user))}
          canRevoke={isAdmin || sameText(scheduleTarget.holder, auth.user)}
          onClose={() => setScheduleTarget(null)}
          onAuthorize={() => {
            setScheduleTarget(null);
            setAllowTarget(scheduleToken);
          }}
          onRevoke={() => {
            setRevokeTarget(scheduleTarget);
          }}
        />
      )}
      {reservationSuccess && !allowTarget && !revokeTarget && !successKey && (
        <ScheduleDetailModal
          title="Reservation created"
          target={reservationSuccess}
          canAuthorize={Boolean(reservationSuccess.id)}
          canShowKey={Boolean(reservationSuccess.id)}
          canRevoke={Boolean(reservationSuccess.id)}
          onClose={() => setReservationSuccess(null)}
          onAuthorize={() => setAllowTarget(reservationSuccess.token)}
          onShowKey={() => showKey(reservationSuccess.id)}
          onRevoke={() => setRevokeTarget(reservationSuccess)}
        />
      )}
      {reserveHint && (
        <ReserveHintModal
          title={reserveHint.title}
          message={reserveHint.message}
          onClose={() => setReserveHint(null)}
        />
      )}
      {successKey && <SuccessKey token={successKey} onClose={() => setSuccessKey("")} />}
      {historyTarget && (
        <HistorySessionModal
          session={historyTarget}
          jobs={historyJobs}
          currentUser={auth.user}
          onClose={() => {
            setHistoryTarget(null);
            setHistoryJobs([]);
          }}
          onSave={saveHistoryResult}
        />
      )}
    </div>
  );
}

function HistoryDashboard({ summary, sessions, servers, filters, loading, loadingMore, nextCursor, onFilters, onLoadMore, onOpen }) {
  const [filterOpen, setFilterOpen] = useState(false);
  const filterRef = useRef(null);
  const errors = historyFilterErrors(filters);
  const errorByRule = new Map(errors.map((item) => [item.id, item.message]));
  const ruleCount = historyRuleCount(filters);
  const cards = [
    ["Reservations", summary?.sessions ?? 0],
    ["Reserved GPU hours", fixedNumber(summary?.reserved_gpu_hours, 1)],
    ["Busy GPU hours", fixedNumber(summary?.busy_gpu_hours, 1)],
    ["Busy ratio", percentLabel(summary?.busy_ratio)],
    ["Average utilization", percentLabel((summary?.average_utilization_percent ?? 0) / 100)],
    ["Telemetry coverage", percentLabel(summary?.telemetry_coverage)],
    ["Jobs", summary?.jobs ?? 0],
  ];

  useEffect(() => {
    if (!filterOpen) return undefined;
    function close(event) {
      if (!filterRef.current?.contains(event.target)) setFilterOpen(false);
    }
    function closeOnEscape(event) {
      if (event.key === "Escape") setFilterOpen(false);
    }
    document.addEventListener("pointerdown", close);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", close);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [filterOpen]);

  function updateRule(groupID, ruleID, nextRule) {
    onFilters({
      groups: filters.groups.map((group) => group.id === groupID
        ? { ...group, rules: group.rules.map((rule) => rule.id === ruleID ? nextRule : rule) }
        : group),
    });
  }

  function removeRule(groupID, ruleID) {
    onFilters({
      groups: filters.groups
        .map((group) => group.id === groupID ? { ...group, rules: group.rules.filter((rule) => rule.id !== ruleID) } : group)
        .filter((group) => group.rules.length > 0),
    });
  }

  function addRule(groupID) {
    if (ruleCount >= 32) return;
    onFilters({
      groups: filters.groups.map((group) => group.id === groupID && group.rules.length < 8
        ? { ...group, rules: [...group.rules, newHistoryRule()] }
        : group),
    });
  }

  function addGroup() {
    if (filters.groups.length >= 8 || ruleCount >= 32) return;
    onFilters({ groups: [...filters.groups, newHistoryGroup()] });
  }

  return (
    <section className="history-page">
      <div className="history-summary-grid">
        {cards.map(([label, value]) => (
          <article className="history-summary-card" key={label}>
            <span>{label}</span><strong>{value}</strong>
          </article>
        ))}
      </div>
      <div className="history-filter-controls">
        <p className="muted history-metric-note">Utilization and VRAM describe the whole GPU during the reservation; bypass workloads may contribute to these values.</p>
        {loading && <span className="muted history-filter-refreshing">Refreshing…</span>}
        <div className="history-filter-menu" ref={filterRef}>
          <button
            type="button"
            className={`small-button history-filter-button ${filterOpen ? "active" : ""}`}
            aria-expanded={filterOpen}
            onClick={() => setFilterOpen((current) => !current)}
          >
            Filters
          </button>
          {filterOpen && (
            <div className="history-filter-popover">
              <div className="history-filter-popover-head">
                <div><strong>Session filters</strong><small>Groups use AND; rules inside a group use OR.</small></div>
                {ruleCount > 0 && <button type="button" className="plain-button" onClick={() => onFilters({ groups: [] })}>Clear all</button>}
              </div>
              <div className="history-filter-groups">
                {filters.groups.map((group, groupIndex) => (
                  <div className="history-filter-group" key={group.id}>
                    {groupIndex > 0 && <span className="history-filter-join">AND</span>}
                    <div className="history-filter-group-head"><strong>Group {groupIndex + 1}</strong><span>Match any rule</span></div>
                    {group.rules.map((rule, ruleIndex) => (
                      <React.Fragment key={rule.id}>
                        {ruleIndex > 0 && <span className="history-filter-or">OR</span>}
                        <HistoryRuleEditor
                          rule={rule}
                          servers={servers}
                          error={errorByRule.get(rule.id)}
                          onChange={(nextRule) => updateRule(group.id, rule.id, nextRule)}
                          onRemove={() => removeRule(group.id, rule.id)}
                        />
                      </React.Fragment>
                    ))}
                    <button type="button" className="history-filter-add" disabled={group.rules.length >= 8 || ruleCount >= 32} onClick={() => addRule(group.id)}>+ Add OR rule</button>
                  </div>
                ))}
                {filters.groups.length === 0 && <div className="history-filter-empty">No filters. All reservation sessions are shown.</div>}
              </div>
              <button type="button" className="small-button" disabled={filters.groups.length >= 8 || ruleCount >= 32} onClick={addGroup}>+ Add AND group</button>
            </div>
          )}
        </div>
      </div>
      <section className="history-list-panel">
        <div className="section-heading">
          <div><h2>Reservation sessions</h2><p className="muted">GPU utilization, observed jobs and user results are retained after the reservation ends.</p></div>
        </div>
        <div className="history-table-scroll">
          <div className="history-table">
            <div className="history-table-head"><span>Session</span><span>Owner</span><span>Window</span><span>GPU</span><span>Utilization</span><span>Jobs</span><span>Status</span></div>
            {sessions.map((session) => {
              const observed = (session.gpu_summaries || []).reduce((sum, gpu) => sum + (gpu.observed_ms || 0), 0);
              const weighted = (session.gpu_summaries || []).reduce((sum, gpu) => sum + (gpu.average_utilization_percent || 0) * (gpu.observed_ms || 0), 0);
              const utilization = observed > 0 ? weighted / observed : null;
              return (
                <button className="history-table-row" key={session.id} onClick={() => onOpen(session.id)}>
                  <span><strong>{session.purpose || "Reservation"}</strong><small>{session.server_name}</small></span>
                  <span>{session.owner}</span>
                  <span>{compactDateTime(session.starts_at)}<small>to {compactDateTime(session.expires_at)}</small></span>
                  <span>{session.gpus?.join(", ") || "—"}</span>
                  <span>{utilization == null ? "—" : `${utilization.toFixed(1)}%`}</span>
                  <span>{session.job_count || 0}</span>
                  <span><em className={`history-status ${session.status}`}>{session.status}</em>{session.history_quality === "partial" && <small>partial telemetry</small>}</span>
                </button>
              );
            })}
            {!loading && sessions.length === 0 && <div className="empty">No reservation history matches these filters.</div>}
            {nextCursor && <button type="button" className="history-load-more" disabled={loadingMore} onClick={onLoadMore}>{loadingMore ? "Loading…" : "Load more"}</button>}
          </div>
        </div>
      </section>
    </section>
  );
}

function HistoryRuleEditor({ rule, servers, error, onChange, onRemove }) {
  const field = historyFilterField(rule.field, servers);
  const operators = historyOperatorsFor(field);
  function changeField(value) {
    const nextField = historyFilterField(value, servers);
    const operator = historyOperatorsFor(nextField)[0].value;
    onChange({ ...rule, field: value, operator, value: historyInitialRuleValue(nextField, operator) });
  }
  function changeOperator(value) {
    onChange({ ...rule, operator: value, value: historyInitialRuleValue(field, value) });
  }
  return (
    <div className={`history-filter-rule ${error ? "invalid" : ""}`}>
      <div className="history-filter-rule-fields">
        <select aria-label="Filter field" value={rule.field} onChange={(event) => changeField(event.target.value)}>
          {historyFilterFields(servers).map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}
        </select>
        <select aria-label="Filter operator" value={rule.operator} onChange={(event) => changeOperator(event.target.value)}>
          {operators.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}
        </select>
        <HistoryRuleValue field={field} rule={rule} onChange={(value) => onChange({ ...rule, value })} />
        <button type="button" className="history-filter-remove" aria-label="Remove filter rule" onClick={onRemove}>×</button>
      </div>
      {error && <small>{error}</small>}
    </div>
  );
}

function HistoryRuleValue({ field, rule, onChange }) {
  if (historyUnaryOperators.has(rule.operator)) return <span className="history-filter-no-value">No value</span>;
  const range = rule.operator === "between" || rule.operator === "overlaps";
  if (range) {
    const values = Array.isArray(rule.value) ? rule.value : ["", ""];
    const type = field.type === "time" || field.type === "window" ? "datetime-local" : "number";
    return (
      <span className="history-filter-range">
        <input type={type} step="any" value={values[0] ?? ""} onChange={(event) => onChange([event.target.value, values[1] ?? ""])} />
        <i>to</i>
        <input type={type} step="any" value={values[1] ?? ""} onChange={(event) => onChange([values[0] ?? "", event.target.value])} />
      </span>
    );
  }
  if (field.options && (rule.operator === "in" || rule.operator === "not_in")) {
    const values = Array.isArray(rule.value) ? rule.value : [];
    return (
      <select multiple aria-label="Filter values" value={values} onChange={(event) => onChange(Array.from(event.target.selectedOptions, (option) => option.value))}>
        {field.options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
      </select>
    );
  }
  if (field.options) {
    return (
      <select aria-label="Filter value" value={rule.value ?? ""} onChange={(event) => onChange(event.target.value)}>
        <option value="">Select…</option>
        {field.options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
      </select>
    );
  }
  const type = field.type === "number" ? "number" : field.type === "time" ? "datetime-local" : "text";
  return <input type={type} step="any" value={rule.value ?? ""} placeholder={field.placeholder || "Value"} onChange={(event) => onChange(event.target.value)} />;
}

function HistorySessionModal({ session, jobs, currentUser, onClose, onSave }) {
  const canEdit = session.result_editable && sameText(session.owner, currentUser);
  const timeline = (session.timeline || []).slice(-180);
  return (
    <Modal title={session.purpose || "Reservation session"} onClose={onClose} className="history-modal">
      <div className="history-detail">
        <div className="history-detail-meta">
          <KeyDetail label="Owner" value={session.owner} />
          <KeyDetail label="Node" value={session.server_name} />
          <KeyDetail label="Window" value={`${dateTimeLabel(session.starts_at)} – ${dateTimeLabel(session.expires_at)}`} />
          <KeyDetail label="GPUs" value={session.gpus?.join(", ") || "—"} />
          <KeyDetail label="Status" value={session.status} />
          <KeyDetail label="Quality" value={session.history_quality} />
        </div>
        <div className="history-gpu-grid">
          {(session.gpu_summaries || []).map((gpu) => (
            <article key={gpu.gpu} className="history-gpu-card">
              <strong>GPU {gpu.gpu}</strong>
              <span>Busy <b>{durationLabel(gpu.busy_ms)}</b></span>
              <span>Average <b>{gpu.average_utilization_percent == null ? "—" : `${gpu.average_utilization_percent.toFixed(1)}%`}</b></span>
              <span>Coverage <b>{percentLabel(gpu.coverage)}</b></span>
              <span>Average VRAM <b>{gpu.average_memory_used_bytes == null ? "—" : formatBytes(gpu.average_memory_used_bytes)}</b></span>
              <span>Peak VRAM <b>{gpu.peak_memory_used_bytes == null ? "—" : formatBytes(gpu.peak_memory_used_bytes)}</b></span>
            </article>
          ))}
        </div>
        {timeline.length > 0 && (
          <section className="history-chart-section">
            <h3>Recent minute utilization</h3>
            <div className="history-chart" title="Latest 180 GPU-minute buckets">
              {timeline.map((point, index) => (
                <span key={`${point.gpu}-${point.minute}-${index}`} style={{ height: `${Math.max(2, point.average_utilization_percent || 0)}%` }} title={`GPU ${point.gpu} · ${compactDateTime(point.minute)} · ${(point.average_utilization_percent || 0).toFixed(1)}%`} />
              ))}
            </div>
          </section>
        )}
        {(session.authorization_scopes || []).length > 0 && (
          <section className="history-jobs">
            <div className="section-heading compact"><div><h3>Authorization scopes</h3><p className="muted">Permissions active during this reservation.</p></div></div>
            {session.authorization_scopes.map((scope, index) => (
              <article className="history-job" key={`${scope.created_at}-${index}`}>
                <div><strong>{scope.mode}</strong><span>{scope.holder}</span></div>
                <code>{scope.command?.length ? scope.command.join(" ") : scope.selector || "—"}</code>
                <small>{compactDateTime(scope.created_at)} → {scope.ended_at ? compactDateTime(scope.ended_at) : scope.expires_at ? compactDateTime(scope.expires_at) : "active"}{scope.end_reason ? ` · ${scope.end_reason}` : ""}</small>
              </article>
            ))}
          </section>
        )}
        <section className="history-jobs">
          <div className="section-heading compact"><div><h3>Observed jobs</h3><p className="muted">Full command lines are visible to every signed-in user and may contain sensitive arguments.</p></div></div>
          {jobs.map((job) => (
            <article className="history-job" key={job.id}>
              <div><strong>{job.source === "rocguard_run" ? "rocguard run" : `${job.mode} process`}</strong><span>{job.gpus?.length ? `GPU ${job.gpus.join(", ")}` : "No GPU observed"}</span></div>
              <code>{job.command?.join(" ") || "—"}</code>
              <small>{job.started_at ? compactDateTime(job.started_at) : "unknown start"}{job.start_precision ? ` (${job.start_precision})` : ""} → {job.finished_at ? compactDateTime(job.finished_at) : "running"}{job.finish_precision ? ` (${job.finish_precision})` : ""}{job.exit_code != null ? ` · exit ${job.exit_code}` : ""}{job.reason ? ` · ${job.reason}` : ""}</small>
            </article>
          ))}
          {jobs.length === 0 && <div className="empty">No authorized jobs were observed in this reservation.</div>}
        </section>
        <HistoryResultForm result={session.result || { version: 0 }} canEdit={canEdit} onSave={onSave} />
      </div>
    </Modal>
  );
}

function HistoryResultForm({ result, canEdit, onSave }) {
  const [outcome, setOutcome] = useState(result.outcome || "");
  const [note, setNote] = useState(result.note || "");
  const [artifactText, setArtifactText] = useState((result.artifacts || []).map((item) => `${item.label} | ${item.url}`).join("\n"));
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  async function submit(event) {
    event.preventDefault();
    if (!canEdit || pending) return;
    const artifacts = artifactText.split("\n").map((line) => line.trim()).filter(Boolean).map((line) => {
      const [label, ...url] = line.split("|");
      return { label: label.trim(), url: url.join("|").trim() };
    });
    setPending(true);
    setError("");
    try {
      await onSave({ outcome: outcome || null, note, artifacts, version: result.version || 0 });
    } catch (err) {
      setError(err.message);
    } finally {
      setPending(false);
    }
  }
  return (
    <form className="history-result" onSubmit={submit}>
      <div><h3>Session result</h3><p className="muted">Visible to every signed-in user. Only the reservation owner can edit it.</p></div>
      {error && <div className="modal-error">{error}</div>}
      <label>Outcome<select disabled={!canEdit} value={outcome} onChange={(event) => setOutcome(event.target.value)}><option value="">Not set</option><option value="success">Success</option><option value="partial">Partial</option><option value="failed">Failed</option><option value="aborted">Aborted</option></select></label>
      <label>Note<textarea disabled={!canEdit} value={note} maxLength={16384} onChange={(event) => setNote(event.target.value)} placeholder="Summary, findings, or follow-up…" /></label>
      <label>Artifacts<textarea disabled={!canEdit} value={artifactText} onChange={(event) => setArtifactText(event.target.value)} placeholder="Model checkpoint | https://…" /></label>
      {(result.artifacts || []).length > 0 && <div className="history-artifacts">{result.artifacts.map((artifact, index) => <a key={`${artifact.url}-${index}`} href={artifact.url} target="_blank" rel="noreferrer">{artifact.label || artifact.url}</a>)}</div>}
      {canEdit && <button className="primary-button" disabled={pending}>{pending ? "Saving…" : "Save result"}</button>}
    </form>
  );
}

const historyUnaryOperators = new Set(["is_empty", "is_not_empty"]);

const historyOperatorSets = {
  text: [
    ["contains", "contains"], ["not_contains", "does not contain"], ["equals", "is"], ["not_equals", "is not"],
    ["is_empty", "is empty"], ["is_not_empty", "is not empty"],
  ],
  enum: [
    ["equals", "is"], ["not_equals", "is not"], ["in", "is any of"], ["not_in", "is none of"],
    ["is_empty", "is empty"], ["is_not_empty", "is not empty"],
  ],
  number: [
    ["equals", "="], ["not_equals", "≠"], ["lt", "<"], ["lte", "≤"], ["gt", ">"], ["gte", "≥"],
    ["between", "between"], ["is_empty", "is empty"], ["is_not_empty", "is not empty"],
  ],
  time: [
    ["after", "after"], ["before", "before"], ["between", "between"],
    ["is_empty", "is empty"], ["is_not_empty", "is not empty"],
  ],
  window: [["overlaps", "overlaps"]],
};

function historyFilterFields(servers = []) {
  const options = (values) => values.map(([value, label]) => ({ value, label }));
  return [
    { value: "purpose", label: "Session name / purpose", type: "text", placeholder: "training" },
    { value: "owner", label: "Owner", type: "text", placeholder: "username" },
    { value: "node", label: "Node", type: "enum", options: servers.map((server) => ({ value: server.id, label: server.name })) },
    { value: "source", label: "Session source", type: "enum", options: options([["web", "Web"], ["cli", "CLI"]]) },
    { value: "status", label: "Status", type: "enum", options: options([["scheduled", "Scheduled"], ["active", "Active"], ["completed", "Completed"], ["revoked", "Revoked"]]) },
    { value: "history_quality", label: "Telemetry quality", type: "enum", options: options([["complete", "Complete"], ["partial", "Partial"]]) },
    { value: "result_outcome", label: "Result outcome", type: "enum", options: options([["success", "Success"], ["partial", "Partial"], ["failed", "Failed"], ["aborted", "Aborted"]]) },
    { value: "created_at", label: "Created time", type: "time" },
    { value: "starts_at", label: "Start time", type: "time" },
    { value: "effective_end", label: "Effective end", type: "time" },
    { value: "session_window", label: "Session window", type: "window" },
    { value: "duration_ms", label: "Duration (hours)", type: "number", factor: hourMs },
    { value: "gpu", label: "Session GPU", type: "number" },
    { value: "gpu_count", label: "GPU count", type: "number" },
    { value: "reserved_ms", label: "Reserved GPU hours", type: "number", factor: hourMs },
    { value: "busy_ms", label: "Busy GPU hours", type: "number", factor: hourMs },
    { value: "average_utilization_percent", label: "Average utilization (%)", type: "number" },
    { value: "busy_ratio", label: "Busy ratio (%)", type: "number", factor: 0.01 },
    { value: "coverage", label: "Telemetry coverage (%)", type: "number", factor: 0.01 },
    { value: "average_vram_bytes", label: "Average VRAM (GiB)", type: "number", factor: 1024 ** 3 },
    { value: "peak_vram_bytes", label: "Peak VRAM (GiB)", type: "number", factor: 1024 ** 3 },
    { value: "job_count", label: "Job count", type: "number" },
    { value: "job.source", label: "Job source", type: "enum", options: options([["rocguard_run", "rocguard run"], ["authorized_process", "Authorized process"]]) },
    { value: "job.mode", label: "Job mode", type: "text" },
    { value: "job.holder", label: "Job holder", type: "text" },
    { value: "job.command", label: "Job command / argv", type: "text", placeholder: "train.py" },
    { value: "job.gpu", label: "Job GPU", type: "number" },
    { value: "job.started_at", label: "Job start time", type: "time" },
    { value: "job.finished_at", label: "Job finish time", type: "time" },
    { value: "job.start_precision", label: "Job start precision", type: "enum", options: options([["exact", "Exact"], ["observed", "Observed"]]) },
    { value: "job.finish_precision", label: "Job finish precision", type: "enum", options: options([["exact", "Exact"], ["observed", "Observed"]]) },
    { value: "job.exit_code", label: "Job exit code", type: "number" },
    { value: "job.end_reason", label: "Job end reason", type: "text" },
  ];
}

function historyFilterField(value, servers = []) {
  const fields = historyFilterFields(servers);
  return fields.find((field) => field.value === value) || fields[0];
}

function historyOperatorsFor(field) {
  return historyOperatorSets[field.type].map(([value, label]) => ({ value, label }));
}

function newHistoryRule() {
  historyFilterID += 1;
  return { id: `history-rule-${historyFilterID}`, field: "purpose", operator: "contains", value: "" };
}

function newHistoryGroup() {
  historyFilterID += 1;
  return { id: `history-group-${historyFilterID}`, rules: [newHistoryRule()] };
}

function historyInitialRuleValue(field, operator) {
  if (historyUnaryOperators.has(operator)) return null;
  if (operator === "between" || operator === "overlaps") return ["", ""];
  if (operator === "in" || operator === "not_in") return [];
  return "";
}

function historyRuleCount(filters) {
  return (filters.groups || []).reduce((sum, group) => sum + (group.rules || []).length, 0);
}

function historyFilterErrors(filters) {
  const errors = [];
  for (const group of filters.groups || []) {
    for (const rule of group.rules || []) {
      const field = historyFilterField(rule.field);
      const allowed = new Set(historyOperatorsFor(field).map((operator) => operator.value));
      let message = "";
      if (!allowed.has(rule.operator)) {
        message = "Choose a valid operator.";
      } else if (!historyUnaryOperators.has(rule.operator)) {
        if (rule.operator === "between" || rule.operator === "overlaps") {
          const values = Array.isArray(rule.value) ? rule.value : [];
          if (values.length !== 2 || values.some((value) => value === "" || value == null)) {
            message = "Enter both range values.";
          } else if (field.type === "number" && (values.some((value) => !Number.isFinite(Number(value))) || Number(values[0]) > Number(values[1]))) {
            message = "Enter an ordered numeric range.";
          } else if ((field.type === "time" || field.type === "window") && (values.some((value) => !Number.isFinite(new Date(value).getTime())) || new Date(values[0]) > new Date(values[1]))) {
            message = "Enter an ordered time range.";
          }
        } else if (rule.operator === "in" || rule.operator === "not_in") {
          if (!Array.isArray(rule.value) || rule.value.length === 0) message = "Select at least one value.";
        } else if (rule.value === "" || rule.value == null) {
          message = "Enter a value.";
        } else if (field.type === "number" && !Number.isFinite(Number(rule.value))) {
          message = "Enter a valid number.";
        } else if (field.type === "time" && !Number.isFinite(new Date(rule.value).getTime())) {
          message = "Enter a valid date and time.";
        }
      }
      if (message) errors.push({ id: rule.id, message });
    }
  }
  return errors;
}

function historySearchExpression(filters) {
  return {
    groups: (filters.groups || []).map((group) => ({
      rules: group.rules.map((rule) => {
        const field = historyFilterField(rule.field);
        const result = { field: rule.field, operator: rule.operator };
        if (historyUnaryOperators.has(rule.operator)) return result;
        if (field.type === "number") {
          const convert = (value) => Number(value) * (field.factor || 1);
          result.value = Array.isArray(rule.value) ? rule.value.map(convert) : convert(rule.value);
        } else if (field.type === "time" || field.type === "window") {
          const convert = (value) => new Date(value).toISOString();
          result.value = Array.isArray(rule.value) ? rule.value.map(convert) : convert(rule.value);
        } else {
          result.value = rule.value;
        }
        return result;
      }),
    })),
  };
}

function percentLabel(value) {
  return Number.isFinite(Number(value)) ? `${(Number(value) * 100).toFixed(1)}%` : "—";
}

function fixedNumber(value, digits) {
  return Number.isFinite(Number(value)) ? Number(value).toFixed(digits) : "0.0";
}

function durationLabel(milliseconds) {
  const totalSeconds = Math.max(0, Math.round(Number(milliseconds || 0) / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours) return `${hours}h ${minutes}m`;
  if (minutes) return `${minutes}m ${seconds}s`;
  return `${seconds}s`;
}

function compactDateTime(value) {
  return value ? new Date(value).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "—";
}

function LoadingScreen() {
  return (
    <div className="login-shell">
      <div className="login-panel">
        <BrandLockup className="login-brand" showMark={false} />
      </div>
    </div>
  );
}

function LoginScreen({ error, registrationEnabled, onLogin, onRegister, onResetError }) {
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ username: "", password: "", confirmPassword: "" });
  const [localError, setLocalError] = useState("");
  const [pending, setPending] = useState(false);

  function switchMode() {
    if (pending) {
      return;
    }
    setCreating((value) => !value);
    setForm((value) => ({ ...value, password: "", confirmPassword: "" }));
    setLocalError("");
    onResetError();
  }

  return (
    <div className="login-shell">
      <form
        className="login-panel"
        onSubmit={async (event) => {
          event.preventDefault();
          if (pending) {
            return;
          }
          setLocalError("");
          if (creating && form.password !== form.confirmPassword) {
            setLocalError("Passwords do not match");
            return;
          }
          setPending(true);
          try {
            if (creating) {
              await onRegister(form);
            } else {
              await onLogin(form);
            }
          } finally {
            setPending(false);
          }
        }}
      >
        <div>
          <BrandLockup className="login-brand" showMark={false} />
          <h1>{creating ? "Create account" : "Sign in"}</h1>
        </div>
        <label>
          Username
          <input
            autoComplete="username"
            value={form.username}
            onChange={(event) => setForm({ ...form, username: event.target.value })}
            placeholder="Username"
            required
          />
        </label>
        <label>
          Password
          <input
            autoComplete={creating ? "new-password" : "current-password"}
            type="password"
            value={form.password}
            onChange={(event) => setForm({ ...form, password: event.target.value })}
            placeholder="Password"
            required
          />
        </label>
        {creating && (
          <label>
            Confirm password
            <input
              autoComplete="new-password"
              type="password"
              value={form.confirmPassword}
              onChange={(event) => setForm({ ...form, confirmPassword: event.target.value })}
              placeholder="Confirm password"
              required
            />
          </label>
        )}
        {(localError || error) && <div className="login-error">{localError || error}</div>}
        <button className="primary-button" disabled={pending}>
          {pending ? (creating ? "Creating account" : "Signing in") : (creating ? "Create account" : "Sign in")}
        </button>
        {registrationEnabled && (
          <div className="login-switch-row">
            <span>{creating ? "Already have an account?" : "New to RocGuard?"}</span>
            <button type="button" className="login-switch-button" onClick={switchMode} disabled={pending}>
              {creating ? "Sign in" : "Create account"}
            </button>
          </div>
        )}
      </form>
    </div>
  );
}

function BrandLockup({ className, showMark = true }) {
  return (
    <div className={className}>
      {showMark && <img className="brand-mark" src="/rocguard-icon.svg" alt="" />}
      <span>RocGuard</span>
    </div>
  );
}

function MenuIcon() {
  return (
    <svg
      className="settings-icon"
      viewBox="0 0 24 24"
      aria-hidden="true"
      focusable="false"
    >
      <path
        d="M4 7h16M4 12h16M4 17h16"
        fill="none"
        stroke="currentColor"
        strokeLinecap="round"
        strokeWidth="2"
      />
    </svg>
  );
}

function GPUCard({ gpu, selected, onClick }) {
  const state = gpu.state || "available";
  const memory = memoryMetric(gpu);
  const utilization = utilizationMetric(gpu);
  return (
    <button className={`gpu-card ${state} ${selected ? "selected" : ""}`} onClick={onClick}>
      <div className="gpu-title-row">
        <span className="checkbox">{selected ? "✓" : ""}</span>
        <h3>GPU {gpu.id}</h3>
        <span className={`status-chip ${state}`}>{statusLabels[state] || state}</span>
      </div>
      <div className="gpu-metrics">
        <MetricLine label="Memory" value={memory.label} percent={memory.percent} />
        <MetricLine label="Utilization" value={utilization.label} percent={utilization.percent} />
      </div>
    </button>
  );
}

function MetricLine({ label, value, percent }) {
  return (
    <div className="metric-line">
      <div className="metric-text">
        <span>{label}</span>
        <strong>{value}</strong>
      </div>
      <div className="metric-track">
        <span className={`metric-fill ${metricTone(percent)}`} style={{ width: `${percent}%` }} />
      </div>
    </div>
  );
}

function Legend() {
  return (
    <div className="legend">
      <span><i className="dot available" />Available</span>
      <span><i className="dot reserved" />Reserved</span>
      <span><i className="dot claimed" />Claimed</span>
    </div>
  );
}

function Schedule({ gpu, allGPUs = [], selected, reservations, onOpen }) {
  const [selectedDay, setSelectedDay] = useState(() => dateInputValue(new Date()));
  const now = new Date();
  const dayStart = parseDateInput(selectedDay);
  const dayEnd = new Date(dayStart.getTime() + dayMs);
  const isToday = selectedDay === dateInputValue(now);
  const timelineStart = isToday ? startOfHour(now) : dayStart;
  const remainingTodayHours = Math.max(1, Math.ceil((dayEnd.getTime() - timelineStart.getTime()) / hourMs));
  const visibleHours = isToday ? Math.max(minCalendarHours, remainingTodayHours) : 24;
  const timelineEnd = new Date(timelineStart.getTime() + visibleHours * hourMs);
  const hourSlots = Array.from(
    { length: visibleHours },
    (_, index) => new Date(timelineStart.getTime() + index * hourMs),
  );
  const targetGPUs = selected.length ? selected : allGPUs.length ? allGPUs : [gpu];
  const gpuReservations = reservations.filter((reservation) => targetGPUs.includes(reservation.gpu));
  const scheduleJobs = groupScheduleReservations(gpuReservations);
  const blocks = layoutScheduleBlocks(
    scheduleJobs
      .map((job) => scheduleBlock(job, timelineStart, timelineEnd))
      .filter(Boolean),
  );
  const colorByUser = reservationColorMap(blocks);
  const timelineWidth = Math.max(...blocks.map((block) => block.timelineWidth || 0), 0);
  const emptyLabel = selected.length === 0
    ? "All GPUs available all day"
    : targetGPUs.length > 1
      ? "All selected GPUs available all day"
      : "Available all day";

  return (
    <section className="schedule-card">
      <div className="section-heading compact">
        <div>
          <h2>GPU schedule</h2>
        </div>
        <div className="schedule-controls">
          <input
            className="date-picker"
            type="date"
            aria-label="Schedule date"
            value={selectedDay}
            onChange={(event) => event.target.value && setSelectedDay(event.target.value)}
          />
          <button type="button" className="small-button" onClick={() => setSelectedDay(dateInputValue(new Date()))}>
            Today
          </button>
        </div>
      </div>
      <div className="day-calendar">
        <div
          className="timeline-canvas"
          style={{
            minWidth: timelineWidth > 0
              ? `${74 + timelineWidth}px`
              : "100%",
          }}
        >
          {hourSlots.map((slot) => (
            <div className="hour-row" key={slot.getTime()}>
              <time>
                <span className="hour-clock">{formatHour(slot.getHours())}</span>
                {dateInputValue(slot) !== dateInputValue(dayStart) && <span className="hour-day-offset">+1</span>}
              </time>
              <span />
            </div>
          ))}
          <div className="timeline-events" style={{ height: `${hourSlots.length * calendarHourHeight}px` }}>
            {blocks.map((block) => (
              <ScheduleBlockButton
                key={block.id}
                block={block}
                colors={colorByUser.get(reservationColorKey(block))}
                onOpen={onOpen}
              />
            ))}
          </div>
          {blocks.length === 0 && <div className="timeline-empty">{emptyLabel}</div>}
        </div>
      </div>
    </section>
  );
}

function ScheduleBlockButton({ block, colors = reservationPalette[0], onOpen }) {
  return (
    <button
      type="button"
      className={`booking-block ${block.compact ? "compact" : ""} ${block.holder ? "has-holder" : ""}`}
      title={`${block.holder ? `${block.holder} · ` : ""}${block.label} · ${timeLabel(block.start)} - ${timeLabel(block.end)}`}
      style={{
        top: block.top,
        height: block.height,
        left: block.left,
        width: block.width,
        "--booking-bg": colors.bg,
        "--booking-border": colors.border,
        "--booking-hover": colors.hover,
        "--booking-text": colors.text,
        "--booking-focus": colors.focus,
        "--holder-space": block.holder ? `${Math.ceil(estimateTextWidth(block.holder, 9) + 12)}px` : "0px",
      }}
      onClick={() => onOpen(block)}
    >
      <div className="booking-topline">
        <strong>{block.label}</strong>
        {block.holder && <span className="booking-holder">{block.holder}</span>}
      </div>
      <span className="booking-time">{timeLabel(block.start)} - {timeLabel(block.end)}</span>
    </button>
  );
}

function ReserveForm({ owner, selected, gpus, reservations, onMissingSelection, onMissingDetails, onConflict, onBusy, onSubmit }) {
  const defaults = defaultWindow();
  const [form, setForm] = useState({
    purpose: "",
    start: defaults.start,
    end: defaults.end,
  });
  const [pending, setPending] = useState(false);
  const targetLabel = selected.length ? ` ${selected.join(", ")}` : "";
  const hasDetails = reservationDetailsComplete(form);
  const conflict = hasDetails ? reservationConflict(selected, reservations, form) : null;
  const busyConflict = hasDetails ? busyGPUConflict(selected, gpus, form) : null;
  const canSubmit = selected.length > 0 && hasDetails && !conflict && !busyConflict;
  function explainBlockedSubmit() {
    if (selected.length === 0) {
      onMissingSelection();
      return;
    }
    if (!hasDetails) {
      onMissingDetails();
      return;
    }
    if (conflict) {
      onConflict(conflict);
      return;
    }
    if (busyConflict) {
      onBusy(busyConflict);
    }
  }
  return (
    <form
      className="reserve-form"
      onSubmit={async (event) => {
        event.preventDefault();
        if (pending) {
          return;
        }
        if (!canSubmit) {
          explainBlockedSubmit();
          return;
        }
        setPending(true);
        try {
          await onSubmit(form);
        } finally {
          setPending(false);
        }
      }}
    >
      <h3>Reserve GPU{targetLabel}</h3>
      <div className="owner-row">
        <span>User</span>
        <strong>{owner}</strong>
      </div>
      <label>Purpose<input value={form.purpose} onChange={(event) => setForm({ ...form, purpose: event.target.value })} placeholder="Training" /></label>
      <label>Start<input type="datetime-local" value={form.start} onChange={(event) => setForm({ ...form, start: event.target.value })} /></label>
      <label>End<input type="datetime-local" value={form.end} onChange={(event) => setForm({ ...form, end: event.target.value })} /></label>
      {conflict && (
        <div className="form-warning">
          GPU {conflict.gpus.join(", ")} already reserved for this window.
        </div>
      )}
      {busyConflict && (
        <div className="form-warning">
          GPU {busyConflict.gpus.join(", ")} {busyConflict.gpus.length > 1 ? "are" : "is"} busy now.
        </div>
      )}
      <div
        className={`submit-guard ${canSubmit && !pending ? "" : "disabled"}`}
        onClick={() => {
          if (!pending && !canSubmit) {
            explainBlockedSubmit();
          }
        }}
      >
        <button className="primary-button" disabled={!canSubmit || pending}>{pending ? "Submitting" : "Submit"}</button>
      </div>
    </form>
  );
}

function KeysView({ tokens, reservations, canCreate, onCreate, onAllow, onShow, onRevoke }) {
  const reservationsByToken = new Map(
    groupScheduleReservations(reservations).map((reservation) => [reservation.id, reservation]),
  );
  return (
    <section className="keys-panel">
      <div className="section-heading">
        <div>
          <h2>Keys</h2>
          <p className="muted">Claim keys and reserved keys are listed without secrets.</p>
        </div>
        {canCreate && <button className="primary-button" onClick={onCreate}>Create claim key</button>}
      </div>
      <div className="key-list">
        {tokens.map((token) => {
          const reservation = token.mode === "reserved" ? reservationsByToken.get(token.id) : null;
          return (
            <div className="key-row" key={token.id}>
              <div className="key-summary">
                <strong>{token.name || "Key ..."}</strong>
                <span className="key-subtitle">{token.mode} · {new Date(token.created_at).toLocaleDateString()}</span>
                {token.mode === "reserved" && (
                  <div className="reserved-key-details">
                    <KeyDetail label="Purpose" value={reservation?.purpose || "--"} />
                    <KeyDetail label="Start" value={reservation ? dateTimeLabel(reservation.starts_at) : "--"} />
                    <KeyDetail label="End" value={reservation ? dateTimeLabel(reservation.expires_at) : "--"} />
                    <KeyDetail label="GPUs" value={reservation?.gpus?.length ? reservation.gpus.join(", ") : "--"} />
                  </div>
                )}
              </div>
              <div className="key-actions">
                <button className="primary-button" onClick={() => onAllow(token)}>Authorize</button>
                <button className="key-button" onClick={() => onShow(token.id)}>Show key</button>
                <button type="button" className="danger-button" onClick={() => onRevoke(token)}>Revoke</button>
              </div>
            </div>
          );
        })}
        {tokens.length === 0 && <div className="empty">No keys yet.</div>}
      </div>
    </section>
  );
}

function KeyDetail({ label, value }) {
  return (
    <div className="reserved-key-detail">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function AllowKeyModal({ token, rules, onClose, onSubmit, onRemove }) {
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [visibleRules, setVisibleRules] = useState(rules);
  const [removedRuleIds, setRemovedRuleIds] = useState(new Set());
  const [selectedRuleId, setSelectedRuleId] = useState("");
  const [addOpen, setAddOpen] = useState(false);

  useEffect(() => {
    setVisibleRules((current) => (
      mergeRules(current, rules).filter((rule) => !removedRuleIds.has(rule.id))
    ));
  }, [rules, removedRuleIds]);

  async function addRule(values) {
    const rule = await onSubmit(values);
    setVisibleRules((current) => mergeRules(current, [rule]));
    setAddOpen(false);
  }

  async function removeSelectedRule() {
    if (!selectedRuleId) {
      return;
    }
    const ruleId = selectedRuleId;
    setPending(true);
    setError("");
    try {
      await onRemove(ruleId);
      setRemovedRuleIds((current) => new Set(current).add(ruleId));
      setVisibleRules((current) => current.filter((rule) => rule.id !== ruleId));
      setSelectedRuleId("");
    } catch (err) {
      setError(err.message);
    } finally {
      setPending(false);
    }
  }

  return (
    <Modal title="Grant GPU access" onClose={onClose} hideClose className="rules-modal">
      <div className="modal-form">
        <div className="owner-row modal-owner">
          <span>Key</span>
          <strong>{token.name || token.id}</strong>
        </div>
        <div className="rules-table">
          <div className="rules-table-header">
            <span>Scope</span>
            <span>By</span>
            <span>Value</span>
          </div>
          <div className="rules-table-body" role="listbox" aria-label="Authorization rules">
            {visibleRules.map((rule) => (
              <button
                type="button"
                className={`rules-table-row ${selectedRuleId === rule.id ? "selected" : ""}`}
                key={rule.id}
                role="option"
                aria-selected={selectedRuleId === rule.id}
                onClick={() => setSelectedRuleId(rule.id)}
              >
                <span>{authorizationRuleScope(rule.mode)}</span>
                <span>{authorizationRuleBy(rule.mode)}</span>
                <strong title={authorizationRuleValue(rule)}>{authorizationRuleValue(rule)}</strong>
              </button>
            ))}
            {visibleRules.length === 0 && <div className="rules-empty">No rules yet.</div>}
          </div>
          <div className="rules-toolbar">
            <button
              type="button"
              className="rules-tool-button"
              onClick={() => {
                setError("");
                setAddOpen(true);
              }}
              aria-label="Add rule"
              title="Add rule"
            >
              +
            </button>
            <span className="rules-toolbar-divider" />
            <button
              type="button"
              className="rules-tool-button"
              onClick={removeSelectedRule}
              disabled={!selectedRuleId || pending}
              aria-label="Remove selected rule"
              title="Remove selected rule"
            >
              {pending ? "…" : "−"}
            </button>
          </div>
        </div>
        {error && <div className="form-warning">{error}</div>}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose} disabled={pending}>Done</button>
        </div>
        {addOpen && <AddRuleModal onClose={() => setAddOpen(false)} onSubmit={addRule} />}
      </div>
    </Modal>
  );
}

function AddRuleModal({ onClose, onSubmit }) {
  const [form, setForm] = useState({ mode: "docker", value: "" });
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const modeFields = {
    docker: { by: "Container name", placeholder: "trainer or trainer-*", key: "container" },
    k8s: { by: "Namespace", placeholder: "training or training-*", key: "namespace" },
    user: { by: "User", placeholder: "alice or team-*", key: "user" },
  };
  const field = modeFields[form.mode] || modeFields.docker;

  async function submit(event) {
    event.preventDefault();
    if (pending) {
      return;
    }
    const value = form.value.trim();
    if (!value) {
      setError("Value is required");
      return;
    }
    setPending(true);
    setError("");
    try {
      await onSubmit({ mode: form.mode, [field.key]: value });
    } catch (err) {
      setError(err.message);
      setPending(false);
    }
  }

  return (
    <Modal title="Add rule" onClose={onClose} hideClose className="add-rule-modal">
      <form className="modal-form" onSubmit={submit}>
        <div className="add-rule-fields">
          <label>
            Scope
            <select
              value={form.mode}
              onChange={(event) => {
                setError("");
                setForm({ mode: event.target.value, value: "" });
              }}
            >
              <option value="docker">Docker</option>
              <option value="k8s">Kubernetes</option>
              <option value="user">Linux</option>
            </select>
          </label>
          <label>
            By
            <input value={field.by} readOnly />
          </label>
          <label>
            Value
            <input
              value={form.value}
              onChange={(event) => setForm({ ...form, value: event.target.value })}
              placeholder={field.placeholder}
              autoFocus
            />
          </label>
        </div>
        {error && <div className="form-warning">{error}</div>}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose} disabled={pending}>Cancel</button>
          <button className="primary-button" disabled={pending}>{pending ? "Adding" : "Add"}</button>
        </div>
      </form>
    </Modal>
  );
}

function authorizationRuleScope(mode) {
  return {
    docker: "Docker",
    k8s: "Kubernetes",
    user: "Linux",
    bare: "Process",
  }[mode] || mode;
}

function authorizationRuleBy(mode) {
  return {
    docker: "Container name",
    k8s: "Namespace",
    user: "User",
    bare: "Command",
  }[mode] || "";
}

function mergeRules(current, incoming) {
  const merged = new Map(current.map((rule) => [rule.id, rule]));
  incoming.filter(Boolean).forEach((rule) => merged.set(rule.id, rule));
  return Array.from(merged.values());
}

function authorizationRuleValue(rule) {
  if (rule.mode === "docker") {
    return rule.container_pattern || rule.container_id || "Docker container";
  }
  if (rule.mode === "k8s") {
    return rule.namespace || "Kubernetes namespace";
  }
  if (rule.mode === "user") {
    return rule.username || `UID ${rule.uid}`;
  }
  if (rule.mode === "bare") {
    return rule.command?.join(" ") || `PID ${rule.root_pid}`;
  }
  return rule.mode;
}

function UsersView({ users, currentUser, onCreate, onDelete }) {
  return (
    <section className="keys-panel users-panel">
      <div className="section-heading">
        <div>
          <h2>Users</h2>
          <p className="muted">View, create, and remove gateway users.</p>
        </div>
        <button className="primary-button" onClick={onCreate}>Create user</button>
      </div>
      <div className="key-list">
        {users.map((user) => (
          <div className="key-row" key={user.username}>
            <div className="key-summary">
              <strong>{user.username}</strong>
              <span className="key-subtitle">
                {user.role} · {new Date(user.created_at).toLocaleDateString()}
              </span>
            </div>
            {!sameText(user.username, currentUser) && (
              <button type="button" className="small-danger-button" onClick={() => onDelete(user)}>
                Delete
              </button>
            )}
          </div>
        ))}
        {users.length === 0 && <div className="empty">No users yet.</div>}
      </div>
    </section>
  );
}

function AddServerModal({ onClose, onSubmit }) {
  const [form, setForm] = useState({ name: "", endpoint: "", root_key: "", tls_skip_verify: false });
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const plaintextHTTP = form.endpoint.trim().toLowerCase().startsWith("http://");
  return (
    <Modal title="Add server" onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={async (event) => {
          event.preventDefault();
          if (pending) {
            return;
          }
          setPending(true);
          setError("");
          try {
            await onSubmit(form);
          } catch (err) {
            setError(err.message);
          } finally {
            setPending(false);
          }
        }}
      >
        <label>Name<input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="prod-cluster-01" /></label>
        <label>Endpoint API<input value={form.endpoint} onChange={(event) => {
          const endpoint = event.target.value;
          setForm({ ...form, endpoint, tls_skip_verify: endpoint.trim().toLowerCase().startsWith("http://") ? false : form.tls_skip_verify });
        }} placeholder="https://server:8192" required /></label>
        <label>Root key<input type="password" value={form.root_key} onChange={(event) => setForm({ ...form, root_key: event.target.value })} required /></label>
        <label className="checkbox-line"><input type="checkbox" checked={form.tls_skip_verify} disabled={plaintextHTTP} onChange={(event) => setForm({ ...form, tls_skip_verify: event.target.checked })} />Skip TLS verify</label>
        {plaintextHTTP && <div className="form-warning">Plaintext HTTP sends the node root key and control traffic unencrypted. The gateway process must explicitly enable ROCGUARD_WEB_ALLOW_INSECURE_NODES.</div>}
        {error && <div className="modal-error">{error}</div>}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose} disabled={pending}>Cancel</button>
          <button className="primary-button" disabled={pending}>{pending ? "Adding" : "Add"}</button>
        </div>
      </form>
    </Modal>
  );
}

function ChangePasswordModal({ onClose, onSubmit }) {
  const [form, setForm] = useState({ current_password: "", new_password: "" });
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  return (
    <Modal title="Change password" onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={async (event) => {
          event.preventDefault();
          if (pending) {
            return;
          }
          setPending(true);
          setError("");
          try {
            await onSubmit(form);
            onClose();
          } catch (err) {
            setError(err.message);
          } finally {
            setPending(false);
          }
        }}
      >
        <label>Current password<input type="password" autoComplete="current-password" value={form.current_password} onChange={(event) => setForm({ ...form, current_password: event.target.value })} required /></label>
        <label>New password<input type="password" autoComplete="new-password" value={form.new_password} onChange={(event) => setForm({ ...form, new_password: event.target.value })} required /></label>
        {error && <div className="modal-error">{error}</div>}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose} disabled={pending}>Cancel</button>
          <button className="primary-button" disabled={pending}>{pending ? "Saving" : "Save"}</button>
        </div>
      </form>
    </Modal>
  );
}

function CreateUserModal({ onClose, onSubmit }) {
  const [form, setForm] = useState({ username: "", password: "", role: "user" });
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  return (
    <Modal title="Create user" onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={async (event) => {
          event.preventDefault();
          if (pending) {
            return;
          }
          setPending(true);
          setError("");
          try {
            await onSubmit(form);
          } catch (err) {
            setError(err.message === "user already exists" ? "Username already exists." : err.message);
          } finally {
            setPending(false);
          }
        }}
      >
        <label>Username<input value={form.username} onChange={(event) => setForm({ ...form, username: event.target.value })} placeholder="researcher" required /></label>
        <label>Password<input type="password" value={form.password} onChange={(event) => setForm({ ...form, password: event.target.value })} placeholder="Password" required /></label>
        <label>Role
          <select value={form.role} onChange={(event) => setForm({ ...form, role: event.target.value })}>
            <option value="user">User</option>
            <option value="admin">Admin</option>
          </select>
        </label>
        {error && <div className="modal-error">{error}</div>}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose} disabled={pending}>Cancel</button>
          <button className="primary-button" disabled={pending}>{pending ? "Creating" : "Create"}</button>
        </div>
      </form>
    </Modal>
  );
}

function DeleteUserModal({ user, onClose, onSubmit }) {
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  return (
    <Modal title="Delete user" onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={async (event) => {
          event.preventDefault();
          if (pending) {
            return;
          }
          setPending(true);
          setError("");
          try {
            await onSubmit();
            onClose();
          } catch (err) {
            setError(err.message);
          } finally {
            setPending(false);
          }
        }}
      >
        <div className="revoke-summary">
          <strong>{user.username}</strong>
          <span>{user.role}</span>
        </div>
        <p className="muted">This removes web access for this user. Existing keys and reservations remain.</p>
        {error && <div className="modal-error">{error}</div>}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose} disabled={pending}>Cancel</button>
          <button className="danger-button" disabled={pending}>{pending ? "Deleting" : "Delete"}</button>
        </div>
      </form>
    </Modal>
  );
}

function ClaimKeyModal({ owner, onClose, onSubmit }) {
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  return (
    <Modal title="Create claim key" onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={async (event) => {
          event.preventDefault();
          if (pending) {
            return;
          }
          setPending(true);
          setError("");
          try {
            await onSubmit();
          } catch (err) {
            setError(err.message);
          } finally {
            setPending(false);
          }
        }}
      >
        <div className="owner-row modal-owner">
          <span>User</span>
          <strong>{owner}</strong>
        </div>
        {error && <div className="modal-error">{error}</div>}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose} disabled={pending}>Cancel</button>
          <button className="primary-button" disabled={pending}>{pending ? "Creating" : "Create"}</button>
        </div>
      </form>
    </Modal>
  );
}

function ScheduleDetailModal({
  title = "Reservation details",
  target,
  canAuthorize,
  canShowKey,
  canRevoke,
  onClose,
  onAuthorize,
  onShowKey,
  onRevoke,
}) {
  return (
    <Modal title={title} onClose={onClose} hideClose>
      <div className="schedule-detail">
        <div className="revoke-summary">
          <strong>{target.label}</strong>
          <span>{timeLabel(target.start)} - {timeLabel(target.end)}</span>
        </div>
        <div className="detail-row">
          <span>GPUs</span>
          <strong>{target.gpus?.length ? target.gpus.join(", ") : "Unknown"}</strong>
        </div>
        {target.holder && (
          <div className="detail-row">
            <span>User</span>
            <strong>{target.holder}</strong>
          </div>
        )}
        {target.purpose && (
          <div className="detail-row">
            <span>Purpose</span>
            <strong>{target.purpose}</strong>
          </div>
        )}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose}>Close</button>
          {canAuthorize && <button type="button" className="primary-button" onClick={onAuthorize}>Authorize</button>}
          {canShowKey && <button type="button" className="key-button" onClick={onShowKey}>Show key</button>}
          {canRevoke && <button type="button" className="danger-button" onClick={onRevoke}>Revoke</button>}
        </div>
      </div>
    </Modal>
  );
}

function RevokeModal({ target, onClose, onSubmit }) {
  const isKey = target.kind === "key";
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const targetLabel = !isKey && target.gpus?.length
    ? `${target.label} · GPU ${target.gpus.join(", ")}`
    : target.label;
  return (
    <Modal title={isKey ? "Revoke key" : "Revoke reservation"} onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={async (event) => {
          event.preventDefault();
          if (pending) {
            return;
          }
          setPending(true);
          setError("");
          try {
            await onSubmit();
          } catch (err) {
            setError(err.message);
          } finally {
            setPending(false);
          }
        }}
      >
        <div className="revoke-summary">
          <strong>{isKey ? target.name || "Key ..." : targetLabel}</strong>
          <span>
            {isKey
              ? `${target.mode} · ${new Date(target.created_at).toLocaleDateString()}`
              : `${timeLabel(target.start)} - ${timeLabel(target.end)}`}
          </span>
        </div>
        <p className="muted">
          {isKey
            ? "This will revoke the key and remove any related reservations or claims."
            : "This will remove this job from every GPU in the reservation."}
        </p>
        {error && <div className="modal-error">{error}</div>}
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose} disabled={pending}>Cancel</button>
          <button className="danger-button" disabled={pending}>{pending ? "Revoking" : "Revoke"}</button>
        </div>
      </form>
    </Modal>
  );
}

function ReserveHintModal({ title, message, onClose }) {
  return (
    <Modal title={title} onClose={onClose} hideClose>
      <div className="modal-note">
        <p className="modal-message">{message}</p>
        <div className="modal-actions">
          <button type="button" className="primary-button" onClick={onClose}>OK</button>
        </div>
      </div>
    </Modal>
  );
}

function SuccessKey({ token, onClose }) {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) {
      return undefined;
    }
    const timer = window.setTimeout(() => setCopied(false), 1200);
    return () => window.clearTimeout(timer);
  }, [copied]);

  async function copyToken() {
    if (!token) {
      return;
    }
    await navigator.clipboard?.writeText(token);
    setCopied(true);
  }

  return (
    <div className="modal-backdrop">
      <section className="success-panel">
        <h2>Reserve Success</h2>
        <p>Your key</p>
        <div className="copy-row">
          <input className="key-output" readOnly spellCheck="false" value={token || "rg ..."} aria-label="Reserved API key" />
          <button
            type="button"
            className={`small-button copy-button ${copied ? "copied" : ""}`}
            onClick={copyToken}
            aria-label={copied ? "Copied" : "Copy key"}
          >
            {copied ? "✓" : "Copy"}
          </button>
        </div>
        <button className="primary-button" onClick={onClose}>Done</button>
      </section>
    </div>
  );
}

function Modal({ title, children, onClose, hideClose = false, className = "" }) {
  return (
    <div className="modal-backdrop">
      <section className={`modal ${className}`.trim()}>
        <header>
          <h2>{title}</h2>
          {!hideClose && <button className="plain-button" onClick={onClose}>Close</button>}
        </header>
        {children}
      </section>
    </div>
  );
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    const err = new Error(data.error || response.statusText);
    err.status = response.status;
    throw err;
  }
  return data;
}

function defaultWindow() {
  const start = new Date(Date.now() + 60 * 60 * 1000);
  start.setMinutes(0, 0, 0);
  const end = new Date(start.getTime() + 2 * 60 * 60 * 1000);
  return { start: datetimeLocal(start), end: datetimeLocal(end) };
}

function reservationDetailsComplete(values) {
  return Boolean(
    values.purpose?.trim() &&
      values.start &&
      values.end,
  );
}

function reservationConflict(selected, reservations = [], values) {
  if (selected.length === 0) {
    return null;
  }
  const start = new Date(values.start);
  const end = new Date(values.end);
  if (!Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || start >= end) {
    return null;
  }
  const selectedSet = new Set(selected);
  const conflicts = [];
  for (const reservation of reservations) {
    if (!selectedSet.has(reservation.gpu)) {
      continue;
    }
    const reservationStart = new Date(reservation.starts_at || reservation.created_at);
    const reservationEnd = new Date(reservation.expires_at);
    if (
      Number.isFinite(reservationStart.getTime()) &&
      Number.isFinite(reservationEnd.getTime()) &&
      reservationStart < end &&
      start < reservationEnd
    ) {
      conflicts.push({ gpu: reservation.gpu, start: reservationStart, end: reservationEnd });
    }
  }
  if (conflicts.length === 0) {
    return null;
  }
  conflicts.sort((left, right) => left.start - right.start || left.gpu - right.gpu);
  const first = conflicts[0];
  return {
    gpus: Array.from(new Set(conflicts.map((item) => item.gpu))).sort((left, right) => left - right),
    start: first.start,
    end: first.end,
  };
}

function busyGPUConflict(selected, gpus = [], values) {
  if (selected.length === 0) {
    return null;
  }
  const start = new Date(values.start);
  const end = new Date(values.end);
  const now = new Date();
  if (
    !Number.isFinite(start.getTime()) ||
    !Number.isFinite(end.getTime()) ||
    start >= end ||
    start > now ||
    end <= now
  ) {
    return null;
  }
  const selectedSet = new Set(selected);
  const busy = gpus
    .filter((gpu) => selectedSet.has(gpu.id) && (gpu.processes || []).some(usesGPUResources))
    .map((gpu) => gpu.id)
    .sort((left, right) => left - right);
  if (busy.length === 0) {
    return null;
  }
  return { gpus: busy };
}

function usesGPUResources(process) {
  return Number(process?.mem_bytes || 0) > 0;
}

function formatReservationError(message) {
  const text = message || "Reservation could not be created.";
  const busyMatch = text.match(/gpu\s+(\d+)\s+is busy:\s+pid=(\d+)/i);
  if (busyMatch) {
    return `GPU ${busyMatch[1]} is busy. A process is already running on this GPU (PID ${busyMatch[2]}). Stop it or choose another GPU/time window.`;
  }
  const overlapMatch = text.match(/gpu\s+(\d+)\s+.*overlaps/i);
  if (overlapMatch) {
    return `GPU ${overlapMatch[1]} already has a reservation in this time window. Choose another GPU or time.`;
  }
  return text;
}

function datetimeLocal(date) {
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
}

function dateInputValue(date) {
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 10);
}

function parseDateInput(value) {
  const [year, month, day] = value.split("-").map(Number);
  return new Date(year, month - 1, day);
}

function startOfHour(date) {
  const out = new Date(date);
  out.setMinutes(0, 0, 0);
  return out;
}

function formatHour(hour) {
  return `${String(hour).padStart(2, "0")}:00`;
}

function groupScheduleReservations(reservations) {
  const groups = new Map();
  for (const reservation of reservations) {
    const key = reservation.group_id || reservationGroupFallbackKey(reservation);
    const existing = groups.get(key);
    if (existing) {
      existing.reservations.push(reservation);
      existing.gpus.push(reservation.gpu);
      continue;
    }
    groups.set(key, {
      id: reservation.group_id || reservation.id,
      label: reservation.purpose || reservation.holder || "Reserved",
      holder: reservation.holder || "",
      purpose: reservation.purpose || "",
      starts_at: reservation.starts_at || reservation.created_at,
      expires_at: reservation.expires_at,
      created_at: reservation.created_at,
      reservations: [reservation],
      gpus: [reservation.gpu],
    });
  }
  return Array.from(groups.values()).map((job) => ({
    ...job,
    gpus: Array.from(new Set(job.gpus)).sort((left, right) => left - right),
  }));
}

function reservationGroupFallbackKey(reservation) {
  return [
    reservation.holder || "",
    reservation.purpose || "",
    reservation.starts_at || reservation.created_at || "",
    reservation.expires_at || "",
    reservation.created_at || "",
  ].join("|");
}

const reservationPalette = [
  { bg: "#fee2e2", border: "#fca5a5", hover: "#fecaca", text: "#991b1b", focus: "rgba(220, 38, 38, 0.2)" },
  { bg: "#ffedd5", border: "#fdba74", hover: "#fed7aa", text: "#9a3412", focus: "rgba(234, 88, 12, 0.2)" },
  { bg: "#fef3c7", border: "#fcd34d", hover: "#fde68a", text: "#854d0e", focus: "rgba(217, 119, 6, 0.22)" },
  { bg: "#dcfce7", border: "#86efac", hover: "#bbf7d0", text: "#166534", focus: "rgba(22, 163, 74, 0.22)" },
  { bg: "#dbeafe", border: "#93c5fd", hover: "#bfdbfe", text: "#1d4ed8", focus: "rgba(37, 99, 235, 0.22)" },
  { bg: "#e0e7ff", border: "#a5b4fc", hover: "#c7d2fe", text: "#3730a3", focus: "rgba(79, 70, 229, 0.22)" },
  { bg: "#fce7f3", border: "#f9a8d4", hover: "#fbcfe8", text: "#9d174d", focus: "rgba(219, 39, 119, 0.2)" },
  { bg: "#ccfbf1", border: "#5eead4", hover: "#99f6e4", text: "#0f766e", focus: "rgba(13, 148, 136, 0.22)" },
  { bg: "#ede9fe", border: "#c4b5fd", hover: "#ddd6fe", text: "#6d28d9", focus: "rgba(124, 58, 237, 0.22)" },
  { bg: "#e0f2fe", border: "#7dd3fc", hover: "#bae6fd", text: "#0369a1", focus: "rgba(2, 132, 199, 0.22)" },
  { bg: "#ecfccb", border: "#bef264", hover: "#d9f99d", text: "#4d7c0f", focus: "rgba(101, 163, 13, 0.22)" },
  { bg: "#fae8ff", border: "#f0abfc", hover: "#f5d0fe", text: "#a21caf", focus: "rgba(192, 38, 211, 0.2)" },
];

function reservationColorMap(blocks) {
  const keys = Array.from(new Set(blocks.map(reservationColorKey))).sort((left, right) => {
    return hashString(left) - hashString(right) || left.localeCompare(right);
  });
  const colors = new Map();
  const used = new Set();
  for (const key of keys) {
    const start = hashString(key) % reservationPalette.length;
    let index = start;
    for (let offset = 0; offset < reservationPalette.length; offset += 1) {
      const candidate = (start + offset) % reservationPalette.length;
      if (!used.has(candidate)) {
        index = candidate;
        break;
      }
    }
    colors.set(key, reservationPalette[index]);
    used.add(index);
  }
  return colors;
}

function reservationColorKey(block) {
  return block.holder || "Unknown user";
}

function hashString(value) {
  let hash = 0;
  for (let index = 0; index < value.length; index += 1) {
    hash = (hash * 31 + value.charCodeAt(index)) >>> 0;
  }
  return hash;
}

function scheduleBlock(job, dayStart, dayEnd) {
  const start = new Date(job.starts_at || job.created_at);
  const end = new Date(job.expires_at);
  if (!Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || start >= dayEnd || end <= dayStart) {
    return null;
  }
  const visibleStart = new Date(Math.max(start.getTime(), dayStart.getTime()));
  const visibleEnd = new Date(Math.min(end.getTime(), dayEnd.getTime()));
  const startMinutes = (visibleStart.getTime() - dayStart.getTime()) / 60000;
  const endMinutes = (visibleEnd.getTime() - dayStart.getTime()) / 60000;
  const durationMinutes = (visibleEnd.getTime() - visibleStart.getTime()) / 60000;
  const heightPx = (durationMinutes / 60) * calendarHourHeight;
  const compact = heightPx < 42;
  return {
    id: job.id,
    gpus: job.gpus,
    holder: job.holder,
    purpose: job.purpose,
    reservations: job.reservations,
    start: visibleStart,
    end: visibleEnd,
    startMinutes,
    endMinutes,
    top: `${(startMinutes / 60) * calendarHourHeight}px`,
    height: `${heightPx}px`,
    compact,
    label: job.label,
  };
}

function layoutScheduleBlocks(blocks) {
  const sorted = [...blocks].sort((left, right) => left.startMinutes - right.startMinutes || left.endMinutes - right.endMinutes);
  const groups = [];
  let group = [];
  let groupEnd = -1;

  for (const block of sorted) {
    if (group.length > 0 && block.startMinutes >= groupEnd) {
      groups.push(group);
      group = [];
      groupEnd = -1;
    }
    group.push(block);
    groupEnd = Math.max(groupEnd, block.endMinutes);
  }
  if (group.length > 0) {
    groups.push(group);
  }

  return groups.flatMap(layoutScheduleGroup);
}

function layoutScheduleGroup(group) {
  const laneEnds = [];
  const assigned = group.map((block) => {
    let lane = laneEnds.findIndex((end) => end <= block.startMinutes);
    if (lane === -1) {
      lane = laneEnds.length;
    }
    laneEnds[lane] = block.endMinutes;
    return { ...block, lane };
  });
  const lanes = Math.max(laneEnds.length, 1);
  const laneWidths = Array.from({ length: lanes }, () => 0);
  for (const block of assigned) {
    laneWidths[block.lane] = Math.max(laneWidths[block.lane], estimateScheduleBlockWidth(block, lanes));
  }
  const laneOffsets = laneWidths.map((_, index) => (
    laneWidths.slice(0, index).reduce((sum, width) => sum + width, 0) + index * scheduleLaneGap
  ));
  const timelineWidth = laneWidths.reduce((sum, width) => sum + width, 0) + (lanes - 1) * scheduleLaneGap;

  return assigned.map((block) => ({
    ...block,
    laneCount: lanes,
    compact: block.compact,
    left: lanes === 1 ? "0" : `${laneOffsets[block.lane]}px`,
    width: lanes === 1 ? `${laneWidths[block.lane]}px` : `${laneWidths[block.lane]}px`,
    timelineWidth,
  }));
}

function estimateScheduleBlockWidth(block, laneCount) {
  const label = block.label || "";
  const holder = block.holder || "";
  const time = `${timeLabel(block.start)} - ${timeLabel(block.end)}`;
  const compact = block.compact;
  const labelWidth = estimateTextWidth(label, compact ? 11 : 12);
  const holderWidth = holder ? estimateTextWidth(holder, compact ? 9 : 9) : 0;
  const timeWidth = estimateTextWidth(time, 11);
  const topLineWidth = labelWidth + (holder ? holderWidth + 6 : 0);
  const contentWidth = compact
    ? labelWidth + timeWidth + (holder ? holderWidth + 12 : 0) + 24
    : Math.max(topLineWidth, timeWidth) + 18;
  if (block.compact) {
    return Math.max(86, Math.ceil(contentWidth));
  }
  return Math.max(104, Math.ceil(contentWidth));
}

function estimateTextWidth(text, fontSize) {
  return text.length * fontSize * 0.56;
}

function memoryMetric(gpu) {
  const used = numberOrNull(gpu.memory_used_bytes) ?? processMemoryBytes(gpu.processes);
  const total = numberOrNull(gpu.memory_total_bytes);
  const hasUsed = used !== null;
  const hasTotal = total !== null && total > 0;
  const label = formatMemoryLabel(hasUsed ? used : null, hasTotal ? total : null);
  const percent = hasUsed && hasTotal ? clamp((used / total) * 100, 0, 100) : 0;
  return { label, percent };
}

function utilizationMetric(gpu) {
  const utilization = numberOrNull(gpu.utilization_percent);
  return {
    label: utilization === null ? "--" : `${Math.round(clamp(utilization, 0, 100))}%`,
    percent: utilization === null ? 0 : clamp(utilization, 0, 100),
  };
}

function metricTone(percent) {
  if (percent >= 85) {
    return "high";
  }
  if (percent >= 50) {
    return "medium";
  }
  return "low";
}

function numberOrNull(value) {
  return Number.isFinite(value) ? value : null;
}

function processMemoryBytes(processes = []) {
  const total = processes.reduce((sum, process) => sum + (Number.isFinite(process.mem_bytes) ? process.mem_bytes : 0), 0);
  return total > 0 ? total : null;
}

function formatMemoryLabel(used, total) {
  if (used === null && total === null) {
    return "--";
  }
  if (used !== null && total !== null) {
    return `${formatBytes(used)} / ${formatBytes(total)}`;
  }
  if (used !== null) {
    return formatBytes(used);
  }
  return `-- / ${formatBytes(total)}`;
}

function formatBytes(value) {
  const gib = value / (1024 ** 3);
  if (gib >= 10) {
    return `${Math.round(gib)} GB`;
  }
  if (gib >= 1) {
    return `${gib.toFixed(1)} GB`;
  }
  return `${Math.round(value / (1024 ** 2))} MB`;
}

function clamp(value, min, max) {
  return Math.min(max, Math.max(min, value));
}

function sameText(left, right) {
  return String(left || "").trim().toLowerCase() === String(right || "").trim().toLowerCase();
}

function timeLabel(value) {
  return new Date(value).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

function dateTimeLabel(value) {
  return new Date(value).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

createRoot(document.getElementById("root")).render(<App />);
