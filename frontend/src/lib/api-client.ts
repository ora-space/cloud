import { create, isAxiosError, type AxiosError, type AxiosRequestConfig } from 'axios'

/**
 * Shared axios instance behind every generated hook in `src/api`.
 *
 * Cross-cutting HTTP policy (base URL, idempotency, interceptors) belongs here
 * so that generated code and hand-written code observe one configuration.
 *
 * Authentication is not a header: the browser holds only the gateway's
 * HttpOnly session cookie, which same-origin requests carry automatically,
 * and the gateway signs the internal credentials the cloud verifies. Nothing
 * in the frontend ever sees or attaches a token.
 */
export const AXIOS_INSTANCE = create({ baseURL: '' })

// POST and DELETE receive a fresh idempotency key when the caller did not
// supply one: the cloud core rejects them without it. The interceptor is
// synchronous so axios keeps dispatching to the adapter immediately — an
// abort must still win the race the way it does without interceptors.
AXIOS_INSTANCE.interceptors.request.use(
  (config) => {
    if (
      (config.method === 'post' || config.method === 'delete') &&
      !config.headers.get('Idempotency-Key')
    ) {
      config.headers.set('Idempotency-Key', crypto.randomUUID())
    }
    return config
  },
  undefined,
  { synchronous: true },
)

type UnauthorizedListener = () => void
const unauthorizedListeners = new Set<UnauthorizedListener>()

/**
 * Registers a listener for any 401 the backend returns, and returns the
 * function that removes it. The session owner subscribes so an expired or
 * revoked cookie ends the session once, instead of every later query failing
 * with the same 401. Listeners run synchronously before the error propagates.
 */
export function onUnauthorized(listener: UnauthorizedListener): () => void {
  unauthorizedListeners.add(listener)
  return () => {
    unauthorizedListeners.delete(listener)
  }
}

/** True when `error` is an HTTP 401 from the shared instance. */
export function isUnauthorizedError(error: unknown): boolean {
  return isAxiosError(error) && error.response?.status === 401
}

/** True for a 403: the caller is known but Cloud refuses them (e.g. a disabled user). */
export function isForbiddenError(error: unknown): boolean {
  return isAxiosError(error) && error.response?.status === 403
}

/** Reads the safe public Fault code without exposing transport or server details. */
export function faultCode(error: unknown): string | undefined {
  if (!isAxiosError<{ code?: string }>(error)) return undefined
  const code = error.response?.data?.code
  return typeof code === 'string' ? code : undefined
}

/**
 * Reads one safe public Fault parameter — the identifier the backend already lets the caller act on,
 * such as the `field` a validation rejection names. Anything but a string is ignored, so a caller
 * never renders transport or server detail.
 */
export function faultParam(error: unknown, key: string): string | undefined {
  if (!isAxiosError<{ params?: Record<string, unknown> }>(error)) return undefined
  const value = error.response?.data?.params?.[key]
  return typeof value === 'string' ? value : undefined
}

AXIOS_INSTANCE.interceptors.response.use(undefined, (error: unknown) => {
  if (isUnauthorizedError(error)) {
    for (const listener of unauthorizedListeners) listener()
  }
  throw error
})

/**
 * Request shape the orval-generated client passes to {@link customInstance}.
 *
 * orval emits `signal: AbortSignal | undefined` rather than omitting the key,
 * so the type must accept an explicit `undefined` under
 * `exactOptionalPropertyTypes`.
 */
export type RequestConfig = Omit<AxiosRequestConfig, 'signal'> & {
  signal?: AbortSignal | undefined
}

/**
 * Promise returned to generated hooks; orval calls `cancel()` on it when a
 * query is torn down before the request settles.
 */
export type CancellablePromise<T> = Promise<T> & { cancel: () => void }

/**
 * orval mutator: executes one request and unwraps the response body.
 *
 * Cancellation has two sources that must both abort the request: the
 * `AbortSignal` react-query passes in `config`, and the legacy `cancel()`
 * method orval attaches to the returned promise.
 */
export const customInstance = <T>(
  config: RequestConfig,
  options?: AxiosRequestConfig,
): CancellablePromise<T> => {
  const controller = new AbortController()
  const signal =
    config.signal === undefined
      ? controller.signal
      : AbortSignal.any([config.signal, controller.signal])
  const promise = AXIOS_INSTANCE<T>({ ...config, ...options, signal }).then(({ data }) => data)
  return Object.assign(promise, { cancel: () => controller.abort() })
}

/** Error type generated hooks expose; the body is the server's `Fault` contract. */
export type ErrorType<Error> = AxiosError<Error>
