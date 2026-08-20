<script lang="ts">
  import { onMount } from "svelte";
  import { authFetch } from "../stores/auth";
  import { inferenceApiKey } from "../stores/auth";
  import { models } from "../stores/api";
  import type { Model } from "../lib/types";

  interface PublicKey {
    id: string;
    name: string;
    prefix: string;
    created: string;
    lastUsed?: string;
    revoked?: boolean;
    models?: string[];
    maxTokens?: number;
  }

  let keys = $state<PublicKey[]>([]);
  let newName = $state("");
  let newModels = $state<string[]>([]);
  let newMaxTokens = $state("");
  let createdSecret = $state("");
  let error = $state("");
  let loading = $state(false);
  let editingId = $state("");
  let editModels = $state<string[]>([]);
  let editMaxTokens = $state("");

  let availableModelIds = $derived(collectModelIds($models));

  function collectModelIds(list: Model[]): string[] {
    const ids = new Set<string>();
    for (const m of list) {
      if (m.unlisted) continue;
      ids.add(m.id);
      for (const alias of m.aliases ?? []) {
        const a = alias.trim();
        if (a) ids.add(a);
      }
    }
    return [...ids].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
  }

  /** Options for a picker: catalog models plus any already saved on the key. */
  function pickerOptions(selected: string[]): string[] {
    const ids = new Set(availableModelIds);
    for (const id of selected) ids.add(id);
    return [...ids].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
  }

  function parseMaxTokens(raw: string): number {
    const t = raw.trim();
    if (!t) return 0;
    const n = Number(t);
    return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
  }

  function toggleModel(selected: string[], id: string): string[] {
    return selected.includes(id) ? selected.filter((m) => m !== id) : [...selected, id];
  }

  async function loadKeys() {
    loading = true;
    error = "";
    try {
      const res = await authFetch("/api/admin/keys");
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      keys = data.keys ?? [];
    } catch (err) {
      error = String(err);
    } finally {
      loading = false;
    }
  }

  async function createKey() {
    error = "";
    createdSecret = "";
    try {
      const res = await authFetch("/api/admin/keys", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: newName || "unnamed",
          models: newModels,
          maxTokens: parseMaxTokens(newMaxTokens),
        }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error || `HTTP ${res.status}`);
      }
      const data = await res.json();
      createdSecret = data.secret;
      newName = "";
      newModels = [];
      newMaxTokens = "";
      await loadKeys();
    } catch (err) {
      error = String(err);
    }
  }

  function startEdit(key: PublicKey) {
    editingId = key.id;
    editModels = [...(key.models ?? [])];
    editMaxTokens = key.maxTokens ? String(key.maxTokens) : "";
  }

  function cancelEdit() {
    editingId = "";
    editModels = [];
    editMaxTokens = "";
  }

  async function saveEdit(id: string) {
    error = "";
    try {
      const res = await authFetch(`/api/admin/keys/${id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          models: editModels,
          maxTokens: parseMaxTokens(editMaxTokens),
        }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error || `HTTP ${res.status}`);
      }
      cancelEdit();
      await loadKeys();
    } catch (err) {
      error = String(err);
    }
  }

  async function revokeKey(id: string) {
    error = "";
    try {
      const res = await authFetch(`/api/admin/keys/${id}`, { method: "DELETE" });
      if (!res.ok && res.status !== 204) throw new Error(`HTTP ${res.status}`);
      if (editingId === id) cancelEdit();
      await loadKeys();
    } catch (err) {
      error = String(err);
    }
  }

  function limitsLabel(key: PublicKey): string {
    const parts: string[] = [];
    if (key.models && key.models.length > 0) {
      parts.push(key.models.join(", "));
    } else {
      parts.push("all models");
    }
    if (key.maxTokens && key.maxTokens > 0) {
      parts.push(`max ${key.maxTokens} tokens`);
    }
    return parts.join(" · ");
  }

  onMount(loadKeys);
</script>

{#snippet modelPicker(selected: string[], onToggle: (id: string) => void)}
  {@const options = pickerOptions(selected)}
  <div class="space-y-1">
    <div class="flex items-center justify-between gap-2">
      <span class="text-sm">Allowed models</span>
      <span class="text-xs text-gray-500 dark:text-gray-400">
        {selected.length === 0 ? "all models" : `${selected.length} selected`}
      </span>
    </div>
    {#if options.length === 0}
      <p class="text-xs text-gray-500 dark:text-gray-400 border border-border rounded px-3 py-2">
        No models available yet. Connect to the dashboard event stream or leave unrestricted.
      </p>
    {:else}
      <div
        class="max-h-48 overflow-y-auto border border-border rounded divide-y divide-border bg-background"
        role="group"
        aria-label="Allowed models"
      >
        {#each options as id (id)}
          <label class="flex items-center gap-2 px-3 py-2 text-sm cursor-pointer hover:bg-gray-50 dark:hover:bg-white/5">
            <input
              type="checkbox"
              class="rounded border-border"
              checked={selected.includes(id)}
              onchange={() => onToggle(id)}
            />
            <span class="font-mono text-xs truncate">{id}</span>
          </label>
        {/each}
      </div>
      <p class="text-xs text-gray-500 dark:text-gray-400">Leave all unchecked to allow every model.</p>
    {/if}
  </div>
{/snippet}

<div class="max-w-3xl mx-auto space-y-6">
  <div>
    <h2 class="text-lg font-semibold">API keys</h2>
    <p class="text-sm text-gray-600 dark:text-gray-400">
      Create keys for inference clients. Optionally restrict models and max token use. The secret is shown once when created.
    </p>
  </div>

  <div class="space-y-2 border border-border rounded p-4">
    <h3 class="font-medium">Playground / client key</h3>
    <p class="text-sm text-gray-600 dark:text-gray-400">
      Paste an inference API key here for playground requests from the browser.
    </p>
    <input
      class="w-full border border-border rounded px-3 py-2 bg-background font-mono text-sm"
      type="password"
      placeholder="sk-ls-..."
      bind:value={$inferenceApiKey}
    />
  </div>

  <div class="space-y-3 border border-border rounded p-4">
    <h3 class="font-medium">Create key</h3>
    <div class="grid gap-3 sm:grid-cols-2">
      <label class="space-y-1 sm:col-span-2">
        <span class="text-sm">Name</span>
        <input class="w-full border border-border rounded px-3 py-2 bg-background" bind:value={newName} placeholder="my-client" />
      </label>
      <div class="sm:col-span-2">
        {@render modelPicker(newModels, (id) => {
          newModels = toggleModel(newModels, id);
        })}
      </div>
      <label class="space-y-1">
        <span class="text-sm">Max tokens per request</span>
        <input
          class="w-full border border-border rounded px-3 py-2 bg-background font-mono text-sm"
          bind:value={newMaxTokens}
          inputmode="numeric"
          placeholder="unlimited"
        />
      </label>
      <div class="flex items-end">
        <button class="rounded bg-indigo-600 text-white px-4 py-2" type="button" onclick={createKey}>Create key</button>
      </div>
    </div>
  </div>

  {#if createdSecret}
    <div class="border border-amber-500 rounded p-3 bg-amber-50 dark:bg-amber-950 space-y-2">
      <p class="text-sm font-medium">Copy this key now — it will not be shown again:</p>
      <code class="block text-xs break-all">{createdSecret}</code>
    </div>
  {/if}

  {#if error}
    <p class="text-sm text-red-600">{error}</p>
  {/if}

  {#if loading}
    <p class="text-sm">Loading...</p>
  {:else}
    <table class="w-full text-sm border-collapse">
      <thead>
        <tr class="text-left border-b border-border">
          <th class="py-2">Name</th>
          <th class="py-2">Prefix</th>
          <th class="py-2">Limits</th>
          <th class="py-2">Created</th>
          <th class="py-2"></th>
        </tr>
      </thead>
      <tbody>
        {#each keys as key (key.id)}
          <tr class="border-b border-border align-top">
            <td class="py-2">{key.name}{key.revoked ? " (revoked)" : ""}</td>
            <td class="py-2 font-mono">{key.prefix}</td>
            <td class="py-2 text-xs text-gray-600 dark:text-gray-400">
              {#if editingId === key.id}
                <div class="space-y-2 min-w-[14rem]">
                  {@render modelPicker(editModels, (id) => {
                    editModels = toggleModel(editModels, id);
                  })}
                  <input
                    class="w-full border border-border rounded px-2 py-1 bg-background font-mono"
                    bind:value={editMaxTokens}
                    inputmode="numeric"
                    placeholder="unlimited tokens"
                  />
                  <div class="flex gap-2">
                    <button class="text-indigo-600 hover:underline" type="button" onclick={() => saveEdit(key.id)}>Save</button>
                    <button class="text-gray-500 hover:underline" type="button" onclick={cancelEdit}>Cancel</button>
                  </div>
                </div>
              {:else}
                {limitsLabel(key)}
              {/if}
            </td>
            <td class="py-2">{new Date(key.created).toLocaleString()}</td>
            <td class="py-2 text-right whitespace-nowrap">
              {#if !key.revoked}
                {#if editingId !== key.id}
                  <button class="text-indigo-600 hover:underline mr-3" type="button" onclick={() => startEdit(key)}>Edit</button>
                {/if}
                <button class="text-red-600 hover:underline" type="button" onclick={() => revokeKey(key.id)}>Revoke</button>
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
