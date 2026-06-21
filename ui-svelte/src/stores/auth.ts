import { get, writable } from "svelte/store";
import { persistentStore } from "./persistent";

export interface AuthSession {
  adminRequired: boolean;
  authenticated: boolean;
  inferenceRequired: boolean;
}

export const adminRequired = writable(false);
export const authenticated = writable(true);
export const inferenceRequired = writable(false);
export const authChecked = writable(false);
export const authError = writable("");

export const inferenceApiKey = persistentStore<string>("inferenceApiKey", "");

export async function refreshAuthSession(): Promise<AuthSession> {
  authError.set("");
  try {
    const response = await fetch("/api/auth/session", { credentials: "include" });
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    const data = (await response.json()) as AuthSession;
    adminRequired.set(data.adminRequired);
    authenticated.set(data.authenticated);
    inferenceRequired.set(data.inferenceRequired);
    authChecked.set(true);
    return data;
  } catch (err) {
    authError.set(String(err));
    authChecked.set(true);
    return { adminRequired: false, authenticated: true, inferenceRequired: false };
  }
}

export async function login(password: string): Promise<void> {
  const response = await fetch("/api/auth/login", {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ password }),
  });
  if (!response.ok) {
    throw new Error("Invalid password");
  }
  authenticated.set(true);
  adminRequired.set(true);
}

export async function logout(): Promise<void> {
  await fetch("/api/auth/logout", { method: "POST", credentials: "include" });
  authenticated.set(false);
}

export function authHeaders(): HeadersInit {
  const headers: Record<string, string> = {};
  const key = get(inferenceApiKey);
  if (key) {
    headers.Authorization = `Bearer ${key}`;
  }
  return headers;
}

export function authFetch(input: RequestInfo, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  const key = get(inferenceApiKey);
  if (key) {
    headers.set("Authorization", `Bearer ${key}`);
  }
  return fetch(input, { ...init, headers, credentials: "include" });
}
