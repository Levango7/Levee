// HTTP API client wrapping axios. All LEVEE frontend calls go through this
// module so that authentication, error normalisation and the /api prefix live
// in one place. The backend is grpc-gateway: every gRPC method is exposed as a
// REST verb under /api/v1/<service>/<method>.
import axios, { AxiosError, AxiosInstance, AxiosRequestConfig, AxiosResponse } from 'axios'

import type { ApiError } from '@/types/levee'

const BASE_URL = '/api/v1'

// Token storage helpers. We keep the token in localStorage so that a page
// refresh preserves the session. The key is namespaced to avoid collisions
// with other apps on the same origin.
const TOKEN_KEY = 'levee.token'

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) || ''
}

export function setToken(token: string): void {
  if (token) {
    localStorage.setItem(TOKEN_KEY, token)
  } else {
    localStorage.removeItem(TOKEN_KEY)
  }
}

// Explicit counterpart to setToken for logout / login-page "clear" actions.
export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
}

// Singleton axios instance. Created once and reused; exporting the instance
// itself would also work, but the per-verb helpers below give us a single
// place to map AxiosError -> ApiError.
const client: AxiosInstance = axios.create({
  baseURL: BASE_URL,
  timeout: 30_000,
  headers: {
    'Content-Type': 'application/json',
  },
})

// Request interceptor: attach the bearer token if present.
client.interceptors.request.use(
  (config) => {
    const token = getToken()
    if (token) {
      config.headers = config.headers ?? {}
      config.headers.Authorization = `Bearer ${token}`
    }
    return config
  },
  (error) => Promise.reject(error),
)

// Response interceptor: unwrap errors into a stable ApiError shape so callers
// do not have to deal with AxiosError internals.
client.interceptors.response.use(
  (response) => response,
  (error: AxiosError) => Promise.reject(normalizeError(error)),
)

function normalizeError(error: AxiosError): ApiError {
  if (error.response) {
    // The backend (grpc-gateway + our handlers) returns {"error": "..."} on
    // failure; older shapes used {message}. Prefer error, then message, then
    // the axios-generated message.
    const data = error.response.data as
      | { error?: string; message?: string; code?: number }
      | undefined
    return {
      code: error.response.status,
      message: data?.error || data?.message || error.message || `HTTP ${error.response.status}`,
      details: data,
    }
  }
  if (error.request) {
    return {
      code: 0,
      message: '网络异常：服务器未响应',
    }
  }
  return {
    code: -1,
    message: error.message || '未知错误',
  }
}

// Generic request helpers. They return the response `data` directly so callers
// can write `const change = await get<Change>('/changes/123')`.
export async function get<T>(url: string, config?: AxiosRequestConfig): Promise<T> {
  const res: AxiosResponse<T> = await client.get<T>(url, config)
  return res.data
}

export async function post<T>(url: string, body?: unknown, config?: AxiosRequestConfig): Promise<T> {
  const res: AxiosResponse<T> = await client.post<T>(url, body, config)
  return res.data
}

export async function put<T>(url: string, body?: unknown, config?: AxiosRequestConfig): Promise<T> {
  const res: AxiosResponse<T> = await client.put<T>(url, body, config)
  return res.data
}

export async function del<T>(url: string, config?: AxiosRequestConfig): Promise<T> {
  const res: AxiosResponse<T> = await client.delete<T>(url, config)
  return res.data
}

// Re-export the raw instance for advanced use cases (e.g. cancel tokens).
export { client as axiosClient }