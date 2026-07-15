import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";

const statusLabels = {
  available: "Available",
  reserved: "Reserved",
  claimed: "Claimed",
};

const showDevWarnings = import.meta.env.DEV;
const calendarHourHeight = 28;
const minCalendarHours = 10;
const scheduleLaneGap = 2;
const hourMs = 60 * 60 * 1000;
const dayMs = 24 * hourMs;

function App() {
  const [auth, setAuth] = useState({ checking: true, authenticated: false, user: "", role: "" });
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
  const [revokeTarget, setRevokeTarget] = useState(null);
  const [scheduleTarget, setScheduleTarget] = useState(null);
  const [reserveHint, setReserveHint] = useState(null);
  const [successKey, setSuccessKey] = useState("");
  const [error, setError] = useState("");
  const [loginError, setLoginError] = useState("");
  const settingsRef = useRef(null);

  useEffect(() => {
    checkSession();
  }, []);

  useEffect(() => {
    if (!auth.authenticated) {
      return undefined;
    }
    refresh();
    const timer = window.setInterval(refresh, 5000);
    return () => window.clearInterval(timer);
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
    } catch {
      setAuth({ checking: false, authenticated: false, user: "", role: "" });
    }
  }

  async function login(values) {
    try {
      const session = await api("/api/login", {
        method: "POST",
        body: JSON.stringify(values),
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
      setSuccessKey("");
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

  async function refresh() {
    try {
      const [serverList, snapshot] = await Promise.all([
        api("/api/servers"),
        api("/api/fleet/snapshot"),
      ]);
      const nextFleet = snapshot.servers || [];
      setServers(serverList);
      setFleet(nextFleet);
      setSelectedServerId((currentId) => {
        if (currentId && serverList.some((server) => server.id === currentId)) {
          return currentId;
        }
        return serverList[0]?.id || nextFleet[0]?.server?.id || "";
      });
    } catch (err) {
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
  const isAdmin = auth.role === "admin";
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
      setSuccessKey(result.token || "");
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
    } catch (err) {
      setError(err.message);
    }
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
      await refresh();
    } catch (err) {
      setError(err.message);
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
              aria-label="Settings"
              aria-expanded={settingsOpen}
              aria-haspopup="menu"
              title="Settings"
              onClick={() => setSettingsOpen((open) => !open)}
            >
              <SettingsIcon />
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
            <p className="eyebrow">Nodes</p>
            <h1>{current?.server?.name || "No server selected"}</h1>
            {!current?.server && <p className="muted">Add a RocGuard node to begin.</p>}
          </div>
          <div className="topbar-actions">
            <button className={view === "gpu" ? "tab active" : "tab"} onClick={() => setView("gpu")}>
              Schedule
            </button>
            <button className={view === "keys" ? "tab active" : "tab"} onClick={() => setView("keys")}>
              Key
            </button>
            {isAdmin && (
              <button className={view === "users" ? "tab active" : "tab"} onClick={() => setView("users")}>
                Users
              </button>
            )}
          </div>
        </header>

        {showDevWarnings && current?.error && <div className="banner">{current.error}</div>}

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
            onCreate={() => setClaimOpen(true)}
            onShow={showKey}
            onRevoke={(token) => setRevokeTarget({ ...token, kind: "key" })}
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
      {claimOpen && <ClaimKeyModal owner={auth.user} onClose={() => setClaimOpen(false)} onSubmit={createClaimKey} />}
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
          canRevoke={isAdmin || sameText(scheduleTarget.holder, auth.user)}
          onClose={() => setScheduleTarget(null)}
          onRevoke={() => {
            setRevokeTarget(scheduleTarget);
          }}
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
    </div>
  );
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

function LoginScreen({ error, onLogin, onRegister, onResetError }) {
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ username: "", password: "", confirmPassword: "" });
  const [localError, setLocalError] = useState("");

  function switchMode() {
    setCreating((value) => !value);
    setForm((value) => ({ ...value, password: "", confirmPassword: "" }));
    setLocalError("");
    onResetError();
  }

  return (
    <div className="login-shell">
      <form
        className="login-panel"
        onSubmit={(event) => {
          event.preventDefault();
          setLocalError("");
          if (creating && form.password !== form.confirmPassword) {
            setLocalError("Passwords do not match");
            return;
          }
          if (creating) {
            onRegister(form);
          } else {
            onLogin(form);
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
        <button className="primary-button">{creating ? "Create account" : "Sign in"}</button>
        <div className="login-switch-row">
          <span>{creating ? "Already have an account?" : "New to RocGuard?"}</span>
          <button type="button" className="login-switch-button" onClick={switchMode}>
            {creating ? "Sign in" : "Create account"}
          </button>
        </div>
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

function SettingsIcon() {
  return (
    <svg
      className="settings-icon"
      viewBox="0 0 24 24"
      aria-hidden="true"
      focusable="false"
    >
      <path
        d="M12 15.25A3.25 3.25 0 1 0 12 8.75a3.25 3.25 0 0 0 0 6.5Z"
        fill="none"
      />
      <path
        d="M19.4 13.5c.08-.49.1-.99.06-1.5.04-.51.02-1.01-.06-1.5l2.04-1.56-2-3.46-2.43.98a8.6 8.6 0 0 0-2.57-1.49L14.08 2h-4.16l-.36 2.97a8.6 8.6 0 0 0-2.57 1.49l-2.43-.98-2 3.46L4.6 10.5c-.08.49-.1.99-.06 1.5-.04.51-.02 1.01.06 1.5l-2.04 1.56 2 3.46 2.43-.98a8.6 8.6 0 0 0 2.57 1.49l.36 2.97h4.16l.36-2.97a8.6 8.6 0 0 0 2.57-1.49l2.43.98 2-3.46-2.04-1.56Z"
        fill="none"
        stroke="currentColor"
        strokeLinecap="round"
        strokeLinejoin="round"
        strokeWidth="1.8"
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
      onSubmit={(event) => {
        event.preventDefault();
        if (!canSubmit) {
          explainBlockedSubmit();
          return;
        }
        onSubmit(form);
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
        className={`submit-guard ${canSubmit ? "" : "disabled"}`}
        onClick={() => {
          if (!canSubmit) {
            explainBlockedSubmit();
          }
        }}
      >
        <button className="primary-button" disabled={!canSubmit}>Submit</button>
      </div>
    </form>
  );
}

function KeysView({ tokens, onCreate, onShow, onRevoke }) {
  return (
    <section className="keys-panel">
      <div className="section-heading">
        <div>
          <h2>Keys</h2>
          <p className="muted">Claim keys and reserved keys are listed without secrets.</p>
        </div>
        <button className="primary-button" onClick={onCreate}>Create claim key</button>
      </div>
      <div className="key-list">
        {tokens.map((token) => (
          <div className="key-row" key={token.id}>
            <div>
              <strong>{token.name || "Key ..."}</strong>
              <span>{token.mode} · {new Date(token.created_at).toLocaleDateString()}</span>
            </div>
            <div className="key-actions">
              <button className="small-button" onClick={() => onShow(token.id)}>Show key</button>
              <button type="button" className="small-danger-button" onClick={() => onRevoke(token)}>Revoke</button>
            </div>
          </div>
        ))}
        {tokens.length === 0 && <div className="empty">No keys yet.</div>}
      </div>
    </section>
  );
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
            <div>
              <strong>{user.username}</strong>
              <span>{user.role} · {new Date(user.created_at).toLocaleDateString()}</span>
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
  return (
    <Modal title="Add server" onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={(event) => {
          event.preventDefault();
          onSubmit(form);
        }}
      >
        <label>Name<input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="prod-cluster-01" /></label>
        <label>Endpoint API<input value={form.endpoint} onChange={(event) => setForm({ ...form, endpoint: event.target.value })} placeholder="https://server:8443" required /></label>
        <label>Root key<input type="password" value={form.root_key} onChange={(event) => setForm({ ...form, root_key: event.target.value })} required /></label>
        <label className="checkbox-line"><input type="checkbox" checked={form.tls_skip_verify} onChange={(event) => setForm({ ...form, tls_skip_verify: event.target.checked })} />Skip TLS verify</label>
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose}>Cancel</button>
          <button className="primary-button">Add</button>
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
          <button type="button" className="small-button" onClick={onClose}>Cancel</button>
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
          <button type="button" className="small-button" onClick={onClose}>Cancel</button>
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
          <button type="button" className="small-button" onClick={onClose}>Cancel</button>
          <button className="danger-button" disabled={pending}>{pending ? "Deleting" : "Delete"}</button>
        </div>
      </form>
    </Modal>
  );
}

function ClaimKeyModal({ owner, onClose, onSubmit }) {
  return (
    <Modal title="Create claim key" onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={(event) => {
          event.preventDefault();
          onSubmit();
        }}
      >
        <div className="owner-row modal-owner">
          <span>User</span>
          <strong>{owner}</strong>
        </div>
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose}>Cancel</button>
          <button className="primary-button">Create</button>
        </div>
      </form>
    </Modal>
  );
}

function ScheduleDetailModal({ target, canRevoke, onClose, onRevoke }) {
  return (
    <Modal title="Reservation details" onClose={onClose} hideClose>
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
          {canRevoke && <button type="button" className="danger-button" onClick={onRevoke}>Revoke</button>}
        </div>
      </div>
    </Modal>
  );
}

function RevokeModal({ target, onClose, onSubmit }) {
  const isKey = target.kind === "key";
  const targetLabel = !isKey && target.gpus?.length
    ? `${target.label} · GPU ${target.gpus.join(", ")}`
    : target.label;
  return (
    <Modal title={isKey ? "Revoke key" : "Revoke reservation"} onClose={onClose} hideClose>
      <form
        className="modal-form"
        onSubmit={(event) => {
          event.preventDefault();
          onSubmit();
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
        <div className="modal-actions">
          <button type="button" className="small-button" onClick={onClose}>Cancel</button>
          <button className="danger-button">Revoke</button>
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

function Modal({ title, children, onClose, hideClose = false }) {
  return (
    <div className="modal-backdrop">
      <section className="modal">
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

createRoot(document.getElementById("root")).render(<App />);
