<script lang="ts">
  import { onMount } from "svelte";
  import { fetchPerformance, fetchPeerMetrics, fetchPoolMetrics, poolMetrics } from "../stores/api";
  import { persistentStore } from "../stores/persistent";
  import type { SysStat, GpuStat, PeerSnapshot, PoolMetricsSnapshot } from "../lib/types";
  import PerformanceChart from "../components/PerformanceChart.svelte";

  const COLORS = [
    "#3b82f6",
    "#ef4444",
    "#10b981",
    "#f59e0b",
    "#8b5cf6",
    "#ec4899",
    "#06b6d4",
    "#84cc16",
    "#f97316",
    "#14b8a6",
    "#a855f7",
    "#e11d48",
    "#0ea5e9",
    "#eab308",
    "#d946ef",
    "#22d3ee",
  ];

  /** Stable series color from a numeric id or string key (order-independent). */
  function stableColor(key: string | number): string {
    if (typeof key === "number") {
      return COLORS[((key % COLORS.length) + COLORS.length) % COLORS.length];
    }
    let hash = 0;
    for (let i = 0; i < key.length; i++) {
      hash = (hash * 31 + key.charCodeAt(i)) | 0;
    }
    return COLORS[((hash % COLORS.length) + COLORS.length) % COLORS.length];
  }

  /** Latest sample per GPU (peers return the full history ring). */
  function latestGpus(stats: GpuStat[]): GpuStat[] {
    const byKey = new Map<string, GpuStat>();
    for (const g of stats) {
      const key = g.uuid || `id:${g.id}`;
      const prev = byKey.get(key);
      if (!prev || new Date(g.timestamp).getTime() >= new Date(prev.timestamp).getTime()) {
        byKey.set(key, g);
      }
    }
    return [...byKey.values()].sort((a, b) => {
      if (a.id !== b.id) return a.id - b.id;
      return (a.uuid || "").localeCompare(b.uuid || "");
    });
  }

  /** Short label; identical AMD product names are disambiguated by PCI slot. */
  function shortGpuName(g: GpuStat): string {
    let name = g.name?.trim() || `GPU ${g.id}`;
    const radeon = name.match(/\[(Radeon [^\]]+)\]/i);
    if (radeon) {
      name = radeon[1];
    } else if (name.length > 48) {
      name = `${name.slice(0, 45)}…`;
    }
    if (g.uuid) {
      name = `${name} (${g.uuid.replace(/^0000:/, "")})`;
    }
    return name;
  }

  function avgCpuPct(cores: number[] | undefined): string {
    if (!cores?.length) return "?";
    const sum = cores.reduce((a, b) => a + b, 0);
    return (sum / cores.length).toFixed(1);
  }

  const WINDOWS = [
    { label: "5 min", ms: 5 * 60 * 1000 },
    { label: "15 min", ms: 15 * 60 * 1000 },
    { label: "1 hr", ms: 60 * 60 * 1000 },
  ] as const;

  const INTERVALS = [
    { label: "Off", ms: 0 },
    { label: "5s", ms: 5000 },
    { label: "10s", ms: 10000 },
    { label: "30s", ms: 30000 },
    { label: "60s", ms: 60000 },
  ] as const;

  let selectedWindow = persistentStore("perf-window", 0);
  let selectedInterval = persistentStore("perf-refresh-interval", 0);
  let collapsedPeers = persistentStore<Record<string, boolean>>("perf-collapsed-peers", {});
  let sysData = $state<SysStat[]>([]);
  let gpuData = $state<GpuStat[]>([]);
  let refreshing = $state(false);

  let peerData = $state<Record<string, PeerSnapshot>>({});
  let peerPollTime = $state<string>("");

  let poolData = $state<PoolMetricsSnapshot | null>(null);

  function isPeerCollapsed(name: string): boolean {
    return !!$collapsedPeers[name];
  }

  function togglePeerCollapsed(name: string) {
    collapsedPeers.update((map) => {
      const next = { ...map };
      if (next[name]) {
        delete next[name];
      } else {
        next[name] = true;
      }
      return next;
    });
  }

  async function loadPeerData() {
    const resp = await fetchPeerMetrics();
    if (resp) {
      peerData = resp.peers ?? {};
      peerPollTime = resp.poll_time ?? "";
    }
  }

  async function loadPoolData() {
    const resp = await fetchPoolMetrics();
    if (resp) {
      poolData = resp;
    }
  }

  let pollTimer: ReturnType<typeof setInterval> | null = null;
  let visible = $state(true);
  let mounted = $state(false);

  function cutoffTime(): number {
    return Date.now() - WINDOWS[$selectedWindow].ms;
  }

  function formatDelta(ts: string, refTime: number): string {
    const diffMs = new Date(ts).getTime() - refTime;
    const diffSec = Math.round(diffMs / 1000);
    const absSec = Math.abs(diffSec);
    const sign = diffSec <= 0 ? "-" : "+";
    if (absSec < 60) return `${sign}${absSec}s`;
    const min = Math.floor(absSec / 60);
    const sec = absSec % 60;
    if (sec === 0) return `${sign}${min}m`;
    return `${sign}${min}:${sec.toString().padStart(2, "0")}`;
  }

  const sysLabels = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length === 0) return [];
    return timeLabels(stats.map((s) => s.timestamp));
  });

  async function loadAll() {
    const resp = await fetchPerformance();
    if (resp) {
      sysData = resp.sys_stats ?? [];
      gpuData = resp.gpu_stats ?? [];
    }
    await Promise.all([loadPoolData(), loadPeerData()]);
  }

  async function loadIncremental() {
    const lastTs = sysData.length > 0 ? sysData[sysData.length - 1].timestamp : undefined;
    const resp = await fetchPerformance(lastTs);
    if (resp) {
      const newSys = resp.sys_stats ?? [];
      const newGpu = resp.gpu_stats ?? [];
      if (newSys.length > 0) {
        sysData = [...sysData, ...newSys];
      }
      if (newGpu.length > 0) {
        gpuData = [...gpuData, ...newGpu];
      }
    }
    await Promise.all([loadPoolData(), loadPeerData()]);
  }

  function startPolling() {
    stopPolling();
    const ms = INTERVALS[$selectedInterval].ms;
    if (ms <= 0) return;
    pollTimer = setInterval(() => {
      if (visible) {
        loadIncremental();
      }
    }, ms);
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  function handleVisibility() {
    visible = !document.hidden;
    if (visible && mounted) {
      loadAll().then(() => startPolling());
    } else {
      stopPolling();
    }
  }

  function handleIntervalChange(i: number) {
    $selectedInterval = i;
    if (visible && mounted) {
      startPolling();
    }
  }

  async function manualRefresh() {
    refreshing = true;
    await loadIncremental();
    refreshing = false;
  }

  $effect(() => {
    return () => {
      stopPolling();
    };
  });

  onMount(() => {
    mounted = true;
    document.addEventListener("visibilitychange", handleVisibility);
    loadAll().then(() => startPolling());

    const unsubPool = poolMetrics.subscribe((snap) => {
      if (snap) poolData = snap;
    });

    return () => {
      mounted = false;
      stopPolling();
      unsubPool();
      document.removeEventListener("visibilitychange", handleVisibility);
    };
  });

  function timeLabels(timestamps: string[]): string[] {
    if (timestamps.length === 0) return [];
    const refTime = new Date(timestamps[timestamps.length - 1]).getTime();
    return timestamps.map((ts) => formatDelta(ts, refTime));
  }

  function buildCpuDatasetsFrom(stats: SysStat[]) {
    if (stats.length === 0) return [];
    const coreCount = stats[0].cpu_util_per_core.length;
    const datasets = [];
    for (let i = 0; i < coreCount; i++) {
      datasets.push({
        label: `Core ${i}`,
        data: stats.map((s) => s.cpu_util_per_core[i] ?? 0),
        borderColor: stableColor(i),
      });
    }
    return datasets;
  }

  function buildMemSwapDatasetsFrom(stats: SysStat[]) {
    if (stats.length === 0) return [];
    return [
      {
        label: "Memory Used %",
        data: stats.map((s) => (s.mem_total_mb > 0 ? (s.mem_used_mb / s.mem_total_mb) * 100 : 0)),
        borderColor: stableColor("mem-used"),
      },
      {
        label: "Swap Used %",
        data: stats.map((s) => (s.swap_total_mb > 0 ? (s.swap_used_mb / s.swap_total_mb) * 100 : 0)),
        borderColor: stableColor("swap-used"),
      },
    ];
  }

  function buildLoadDatasetsFrom(stats: SysStat[]) {
    if (stats.length === 0) return [];
    return [
      { label: "1 min", data: stats.map((s) => s.load_avg_1), borderColor: stableColor("load-1") },
      { label: "5 min", data: stats.map((s) => s.load_avg_5), borderColor: stableColor("load-5") },
      { label: "15 min", data: stats.map((s) => s.load_avg_15), borderColor: stableColor("load-15") },
    ];
  }

  // --- System charts (filtered by time window) ---

  const filteredSysStats = $derived(sysData.filter((s) => new Date(s.timestamp).getTime() >= cutoffTime()));

  const cpuDatasets = $derived(buildCpuDatasetsFrom(filteredSysStats));
  const memSwapDatasets = $derived(buildMemSwapDatasetsFrom(filteredSysStats));

  const latestMemSwap = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length === 0) return null;
    const s = stats[stats.length - 1];
    return {
      mem_total_mb: s.mem_total_mb,
      mem_used_mb: s.mem_used_mb,
      mem_used_pct: ((s.mem_used_mb / s.mem_total_mb) * 100).toFixed(1),
      swap_total_mb: s.swap_total_mb,
      swap_used_mb: s.swap_used_mb,
      swap_used_pct: s.swap_total_mb > 0 ? ((s.swap_used_mb / s.swap_total_mb) * 100).toFixed(1) : null,
    };
  });

  const loadDatasets = $derived(buildLoadDatasetsFrom(filteredSysStats));

  const netBandwidthDatasets = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length < 2) return [];

    const ifaceNames = new Set<string>();
    for (const s of stats) {
      for (const n of s.net_io ?? []) {
        ifaceNames.add(n.name);
      }
    }

    const interfaces = [...ifaceNames].sort();
    if (interfaces.length === 0) return [];

    const datasets: { label: string; data: number[]; borderColor: string }[] = [];

    for (const iface of interfaces) {
      const recvData: number[] = [];
      const sentData: number[] = [];

      for (let i = 1; i < stats.length; i++) {
        const prev = stats[i - 1];
        const curr = stats[i];
        const prevIO = (prev.net_io ?? []).find((n) => n.name === iface);
        const currIO = (curr.net_io ?? []).find((n) => n.name === iface);

        if (!prevIO || !currIO) {
          recvData.push(0);
          sentData.push(0);
          continue;
        }

        const dtMs = new Date(curr.timestamp).getTime() - new Date(prev.timestamp).getTime();
        if (dtMs <= 0) {
          recvData.push(0);
          sentData.push(0);
          continue;
        }

        const dtSec = dtMs / 1000;
        recvData.push((((currIO.bytes_recv - prevIO.bytes_recv) / dtSec) * 8) / 1_000_000);
        sentData.push((((currIO.bytes_sent - prevIO.bytes_sent) / dtSec) * 8) / 1_000_000);
      }

      datasets.push({
        label: `${iface} in`,
        data: recvData,
        borderColor: stableColor(`${iface}:in`),
      });
      datasets.push({
        label: `${iface} out`,
        data: sentData,
        borderColor: stableColor(`${iface}:out`),
      });
    }

    return datasets;
  });

  const netBandwidthLabels = $derived.by(() => {
    const stats = filteredSysStats;
    if (stats.length < 2) return [];
    const refTime = new Date(stats[stats.length - 1].timestamp).getTime();
    return stats.slice(1).map((s) => formatDelta(s.timestamp, refTime));
  });

  // --- GPU charts (filtered by time window) ---

  const filteredGpuStats = $derived(gpuData.filter((g) => new Date(g.timestamp).getTime() >= cutoffTime()));

  const hasGpuData = $derived(gpuData.length > 0);

  const gpuLabels = $derived.by(() => {
    const seen = new Set<string>();
    const labels: string[] = [];
    const stats = filteredGpuStats;
    if (stats.length === 0) return [];
    const refTime = new Date(stats[stats.length - 1].timestamp).getTime();
    for (const g of stats) {
      const label = formatDelta(g.timestamp, refTime);
      if (!seen.has(label)) {
        seen.add(label);
        labels.push(label);
      }
    }
    return labels;
  });

  function buildGpuDatasets(
    stats: GpuStat[],
    field: keyof Pick<
      GpuStat,
      "gpu_util_pct" | "mem_util_pct" | "temp_c" | "vram_temp_c" | "power_draw_w" | "clock_mhz" | "mem_clock_mhz"
    >,
  ) {
    if (stats.length === 0) return [];

    const byKey = new Map<string, { id: number; name: string; values: number[] }>();
    for (const g of stats) {
      const key = g.uuid || `id:${g.id}`;
      if (!byKey.has(key)) {
        byKey.set(key, { id: g.id, name: shortGpuName(g), values: [] });
      }
      byKey.get(key)!.values.push(g[field] as number);
    }

    // Sort by id (then key) and color by stable key so series stay consistent
    // across refreshes even when sample arrival order changes.
    const entries = [...byKey.entries()].sort(([ka, a], [kb, b]) => {
      if (a.id !== b.id) return a.id - b.id;
      return ka.localeCompare(kb);
    });
    return entries.map(([key, entry]) => ({
      label: entry.name || `GPU ${entry.id}`,
      data: entry.values,
      borderColor: stableColor(key),
    }));
  }

  const gpuUtilDatasets = $derived(buildGpuDatasets(filteredGpuStats, "gpu_util_pct"));
  const gpuMemDatasets = $derived(buildGpuDatasets(filteredGpuStats, "mem_util_pct"));
  const gpuTempDatasets = $derived(buildGpuDatasets(filteredGpuStats, "temp_c"));
  const gpuVramTempDatasets = $derived(buildGpuDatasets(filteredGpuStats, "vram_temp_c"));
  const gpuPowerDatasets = $derived(buildGpuDatasets(filteredGpuStats, "power_draw_w"));
  const gpuClockDatasets = $derived(buildGpuDatasets(filteredGpuStats, "clock_mhz"));
  const gpuMemClockDatasets = $derived(buildGpuDatasets(filteredGpuStats, "mem_clock_mhz"));
  const hasVramTemp = $derived(filteredGpuStats.some((g) => g.vram_temp_c > 0));
  const hasGpuClock = $derived(filteredGpuStats.some((g) => g.clock_mhz > 0));
  const hasMemClock = $derived(filteredGpuStats.some((g) => g.mem_clock_mhz > 0));

  const peerCharts = $derived.by(() => {
    const cutoff = cutoffTime();
    return Object.values(peerData).map((peer) => {
      const sys = (peer.sys_stats ?? []).filter((s) => new Date(s.timestamp).getTime() >= cutoff);
      const gpu = (peer.gpu_stats ?? []).filter((g) => new Date(g.timestamp).getTime() >= cutoff);
      const latestSys = sys.length > 0 ? sys[sys.length - 1] : null;
      const sysLabelList = timeLabels(sys.map((s) => s.timestamp));
      const gpuLabelSeen = new Set<string>();
      const gpuLabelList: string[] = [];
      if (gpu.length > 0) {
        const refTime = new Date(gpu[gpu.length - 1].timestamp).getTime();
        for (const g of gpu) {
          const label = formatDelta(g.timestamp, refTime);
          if (!gpuLabelSeen.has(label)) {
            gpuLabelSeen.add(label);
            gpuLabelList.push(label);
          }
        }
      }
      return {
        peer,
        latestSys,
        latestGpu: latestGpus(peer.gpu_stats ?? []),
        sysLabels: sysLabelList,
        gpuLabels: gpuLabelList,
        cpuDatasets: buildCpuDatasetsFrom(sys),
        memDatasets: buildMemSwapDatasetsFrom(sys),
        loadDatasets: buildLoadDatasetsFrom(sys),
        gpuUtil: buildGpuDatasets(gpu, "gpu_util_pct"),
        gpuMem: buildGpuDatasets(gpu, "mem_util_pct"),
        gpuTemp: buildGpuDatasets(gpu, "temp_c"),
        gpuPower: buildGpuDatasets(gpu, "power_draw_w"),
        gpuClock: buildGpuDatasets(gpu, "clock_mhz"),
        hasSys: sys.length > 0,
        hasGpu: gpu.length > 0,
        hasClock: gpu.some((g) => g.clock_mhz > 0),
      };
    });
  });
</script>

<div class="space-y-6">
  <div class="flex items-center justify-between">
    <h2 class="text-xl font-semibold text-txtmain">Performance (Experimental)</h2>
    <div class="flex items-center gap-4">
      <div class="flex items-center gap-1">
        {#each WINDOWS as win, i}
          <button
            class="btn btn--sm"
            class:bg-primary={$selectedWindow === i}
            class:text-btn-primary-text={$selectedWindow === i}
            onclick={() => ($selectedWindow = i)}
          >
            {win.label}
          </button>
        {/each}
      </div>
      <div class="flex items-center gap-1">
        <span class="text-xs text-txtsecondary mr-1">Refresh:</span>
        {#each INTERVALS as intv, i}
          <button
            class="btn btn--sm"
            class:bg-primary={$selectedInterval === i}
            class:text-btn-primary-text={$selectedInterval === i}
            onclick={() => handleIntervalChange(i)}
          >
            {intv.label}
          </button>
        {/each}
      </div>
      <button class="btn btn--sm p-1" title="Refresh" onclick={manualRefresh} disabled={refreshing}>
        <svg
          xmlns="http://www.w3.org/2000/svg"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          class="w-4 h-4"
          class:animate-spin={refreshing}
        >
          <path d="M21 12a9 9 0 0 0-9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" />
          <path d="M3 3v5h5" />
          <path d="M3 12a9 9 0 0 0 9 9 9.75 9.75 0 0 0 6.74-2.74L21 16" />
          <path d="M16 16h5v5" />
        </svg>
      </button>
    </div>
  </div>
  <p class="text-sm text-txtsecondary">
    This is an experimental feature. Please use <a
      class="underline hover:text-txtmain"
      href="https://github.com/mostlygeek/llama-swap/discussions/771">discussion #771</a
    > for instructions and to share feedback.
  </p>

  <!-- GPU Section -->
  <section class="space-y-4">
    <h3 class="text-lg font-medium text-txtmain">GPU</h3>
    {#if !hasGpuData}
      <p class="text-txtsecondary card p-4">No GPU data available</p>
    {:else}
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <PerformanceChart
          title="GPU Utilization (%)"
          labels={gpuLabels}
          datasets={gpuUtilDatasets}
          yMin={0}
          yMax={100}
          yLabel="%"
        />
        <PerformanceChart
          title="GPU Memory Utilization (%)"
          labels={gpuLabels}
          datasets={gpuMemDatasets}
          yMin={0}
          yMax={100}
          yLabel="%"
        />
        <PerformanceChart
          title="GPU Temperature (°C)"
          labels={gpuLabels}
          datasets={gpuTempDatasets}
          yMin={0}
          yLabel="°C"
        />
        {#if hasVramTemp}
          <PerformanceChart
            title="GPU VRAM Temperature (°C)"
            labels={gpuLabels}
            datasets={gpuVramTempDatasets}
            yMin={0}
            yLabel="°C"
          />
        {/if}
        <PerformanceChart
          title="GPU Power Draw (W)"
          labels={gpuLabels}
          datasets={gpuPowerDatasets}
          yMin={0}
          yLabel="W"
        />
        {#if hasGpuClock}
          <PerformanceChart
            title="GPU Clock (MHz)"
            labels={gpuLabels}
            datasets={gpuClockDatasets}
            yMin={0}
            yLabel="MHz"
          />
        {/if}
        {#if hasMemClock}
          <PerformanceChart
            title="GPU Memory Clock (MHz)"
            labels={gpuLabels}
            datasets={gpuMemClockDatasets}
            yMin={0}
            yLabel="MHz"
          />
        {/if}
      </div>
    {/if}
  </section>

  <!-- System Section -->
  <section class="space-y-4">
    <h3 class="text-lg font-medium text-txtmain">System</h3>
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      <PerformanceChart
        title="CPU Utilization (%)"
        labels={sysLabels}
        datasets={cpuDatasets}
        yMin={0}
        yMax={100}
        yLabel="%"
        showLegend={false}
      />
      <div>
        <PerformanceChart
          title="Memory & Swap Usage (%)"
          labels={sysLabels}
          datasets={memSwapDatasets}
          yMin={0}
          yMax={100}
          yLabel="%"
        />
        {#if latestMemSwap}
          <div class="flex items-center justify-center gap-4 text-xs text-txtsecondary mt-1 px-4">
            <span
              >Mem: <span class="text-txtmain font-medium"
                >{latestMemSwap.mem_used_mb.toLocaleString()} / {latestMemSwap.mem_total_mb.toLocaleString()} MB ({latestMemSwap.mem_used_pct}%)</span
              ></span
            >
            {#if latestMemSwap.swap_used_pct !== null}
              <span
                >Swap: <span class="text-txtmain font-medium"
                  >{latestMemSwap.swap_used_mb.toLocaleString()} / {latestMemSwap.swap_total_mb.toLocaleString()} MB ({latestMemSwap.swap_used_pct}%)</span
                ></span
              >
            {/if}
          </div>
        {/if}
      </div>
      <PerformanceChart title="Load Average" labels={sysLabels} datasets={loadDatasets} yMin={0} />
      {#if netBandwidthDatasets.length > 0}
        <PerformanceChart
          title="Network Bandwidth (Mbit/s)"
          labels={netBandwidthLabels}
          datasets={netBandwidthDatasets}
          yMin={0}
          yLabel="Mbit/s"
          showLegend={false}
        />
      {/if}
    </div>
  </section>

  <!-- Pool Backend Load Section -->
  {#if poolData?.models?.length}
    <section class="space-y-4">
      <h3 class="text-lg font-medium text-txtmain">Pool Backends</h3>
      <p class="text-sm text-txtsecondary">
        Live load-balancer state for always-on upstream backends. Updated: {poolData.timestamp ? new Date(poolData.timestamp).toLocaleTimeString() : "never"}.
      </p>
      {#each poolData.models as model}
        <div class="space-y-2">
          <div class="flex items-baseline gap-2">
            <h4 class="font-medium text-txtmain">{model.model_id}</h4>
            <span class="text-xs text-txtsecondary">{model.strategy} · {model.affinity_sessions} sticky session{model.affinity_sessions === 1 ? "" : "s"}</span>
          </div>
          <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
            {#each model.backends as backend}
              <div class="card p-4 space-y-2">
                <div class="flex items-center justify-between gap-2">
                  <span class="font-medium text-txtmain truncate" title={backend.proxy}>#{backend.id} {backend.proxy}</span>
                  <span class="text-xs px-2 py-0.5 rounded-full shrink-0" class:bg-green-900={backend.healthy} class:text-green-300={backend.healthy} class:bg-red-900={!backend.healthy} class:text-red-300={!backend.healthy}>
                    {backend.healthy ? "healthy" : "down"}
                  </span>
                </div>
                <div class="text-xs text-txtsecondary space-y-1">
                  <p>Kind: {backend.kind ?? "static"}{#if backend.state} · {backend.state}{/if}</p>
                  <p>In-flight: {backend.inflight}</p>
                  <p>Sticky sessions: {backend.affinity_sessions}</p>
                  <p>Context size: {backend.context_size > 0 ? backend.context_size.toLocaleString() : "unknown"}</p>
                </div>
              </div>
            {/each}
          </div>
        </div>
      {/each}
    </section>
  {/if}

  <!-- Peer Performance Section -->
  {#if peerCharts.length > 0}
    <section class="space-y-6">
      <div>
        <h3 class="text-lg font-medium text-txtmain">Peers</h3>
        <p class="text-sm text-txtsecondary">
          Live performance from upstream llama-swap sidecars. Last poll: {peerPollTime
            ? new Date(peerPollTime).toLocaleTimeString()
            : "never"}.
        </p>
      </div>
      {#each peerCharts as view}
        {@const collapsed = isPeerCollapsed(view.peer.peer_name)}
        <div class="card p-4 space-y-4">
          <button
            type="button"
            class="w-full flex items-center gap-2 text-left"
            onclick={() => togglePeerCollapsed(view.peer.peer_name)}
            aria-expanded={!collapsed}
          >
            <svg
              class="w-4 h-4 shrink-0 text-txtsecondary transition-transform {collapsed ? '-rotate-90' : ''}"
              fill="none"
              stroke="currentColor"
              viewBox="0 0 24 24"
              aria-hidden="true"
            >
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"></path>
            </svg>
            <h4 class="font-medium text-txtmain flex-1 truncate">{view.peer.peer_name}</h4>
            {#if collapsed && view.latestSys}
              <span class="hidden sm:inline text-xs text-txtsecondary truncate mr-2">
                CPU {avgCpuPct(view.latestSys.cpu_util_per_core)}% · RAM {((view.latestSys.mem_used_mb /
                  view.latestSys.mem_total_mb) *
                  100).toFixed(0)}% · {view.latestGpu.length} GPU{view.latestGpu.length === 1 ? "" : "s"}
              </span>
            {/if}
            <span
              class="text-xs px-2 py-0.5 rounded-full shrink-0"
              class:bg-green-900={view.peer.success}
              class:text-green-300={view.peer.success}
              class:bg-red-900={!view.peer.success}
              class:text-red-300={!view.peer.success}
            >
              {view.peer.success ? "connected" : "error"}
            </span>
          </button>

          {#if !collapsed}
            {#if view.peer.error}
              <p class="text-xs text-red-400">{view.peer.error}</p>
            {/if}
            {#if view.latestSys}
              <div class="flex flex-wrap gap-4 text-xs text-txtsecondary">
                <span>CPU: <span class="text-txtmain font-medium">{avgCpuPct(view.latestSys.cpu_util_per_core)}%</span></span>
                <span
                  >RAM:
                  <span class="text-txtmain font-medium"
                    >{((view.latestSys.mem_used_mb / view.latestSys.mem_total_mb) * 100).toFixed(1)}% ({view.latestSys
                      .mem_used_mb} / {view.latestSys.mem_total_mb} MB)</span
                  ></span
                >
                <span
                  >Load:
                  <span class="text-txtmain font-medium"
                    >{view.latestSys.load_avg_1.toFixed(2)} / {view.latestSys.load_avg_5.toFixed(2)} / {view.latestSys.load_avg_15.toFixed(
                      2,
                    )}</span
                  ></span
                >
              </div>
            {/if}
            {#if view.latestGpu.length > 0}
              <div class="text-xs text-txtsecondary space-y-1">
                {#each view.latestGpu as gpu}
                  <p
                    >{shortGpuName(gpu)}: {gpu.gpu_util_pct.toFixed(1)}% util, {gpu.temp_c}°C, {(gpu.mem_total_mb > 0
                      ? (gpu.mem_used_mb / gpu.mem_total_mb) * 100
                      : 0
                    ).toFixed(1)}% VRAM{#if gpu.clock_mhz > 0}, {gpu.clock_mhz} MHz{/if}</p
                  >
                {/each}
              </div>
            {/if}

            {#if view.hasGpu}
              <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
                <PerformanceChart
                  title="GPU Utilization (%)"
                  labels={view.gpuLabels}
                  datasets={view.gpuUtil}
                  yMin={0}
                  yMax={100}
                  yLabel="%"
                />
                <PerformanceChart
                  title="GPU Memory Utilization (%)"
                  labels={view.gpuLabels}
                  datasets={view.gpuMem}
                  yMin={0}
                  yMax={100}
                  yLabel="%"
                />
                <PerformanceChart
                  title="GPU Temperature (°C)"
                  labels={view.gpuLabels}
                  datasets={view.gpuTemp}
                  yMin={0}
                  yLabel="°C"
                />
                <PerformanceChart
                  title="GPU Power Draw (W)"
                  labels={view.gpuLabels}
                  datasets={view.gpuPower}
                  yMin={0}
                  yLabel="W"
                />
                {#if view.hasClock}
                  <PerformanceChart
                    title="GPU Clock (MHz)"
                    labels={view.gpuLabels}
                    datasets={view.gpuClock}
                    yMin={0}
                    yLabel="MHz"
                  />
                {/if}
              </div>
            {:else if view.peer.success}
              <p class="text-xs text-txtsecondary">No GPU history from this peer yet.</p>
            {/if}

            {#if view.hasSys}
              <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
                <PerformanceChart
                  title="CPU Utilization (%)"
                  labels={view.sysLabels}
                  datasets={view.cpuDatasets}
                  yMin={0}
                  yMax={100}
                  yLabel="%"
                  showLegend={false}
                />
                <PerformanceChart
                  title="Memory & Swap Usage (%)"
                  labels={view.sysLabels}
                  datasets={view.memDatasets}
                  yMin={0}
                  yMax={100}
                  yLabel="%"
                />
                <PerformanceChart title="Load Average" labels={view.sysLabels} datasets={view.loadDatasets} yMin={0} />
              </div>
            {/if}
          {/if}
        </div>
      {/each}
    </section>
  {/if}
</div>
