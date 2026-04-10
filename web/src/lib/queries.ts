import { useCallback } from 'react';
import { useQuery, type UseQueryOptions } from '@tanstack/react-query';

import { api } from './api';
import { useAuth } from './auth';

// useApi is the canonical way panels fetch data. It binds the
// current auth token, calls api(...), and surfaces the result via
// TanStack Query so caching, refetching, and loading states are
// uniform across the codebase.
//
// Usage:
//
//   const { data, isLoading, error } = useApi<SessionList>(
//     ['sessions'],
//     '/api/v1/sessions',
//   );
export function useApi<T>(
  key: readonly unknown[],
  path: string,
  opts?: Omit<UseQueryOptions<T, Error>, 'queryKey' | 'queryFn'>,
) {
  const { token } = useAuth();
  return useQuery<T, Error>({
    queryKey: key,
    queryFn: () => api<T>(path, token),
    enabled: !!token,
    ...opts,
  });
}

// useAuthFetch returns a raw fetch wrapper with the Bearer token
// pre-injected. Use for mutations (POST/PUT/DELETE) where TanStack
// Query's queryFn pattern doesn't fit.
export function useAuthFetch() {
  const { token } = useAuth();
  return useCallback(
    (path: string, init?: RequestInit) => {
      const headers: Record<string, string> = {
        ...(init?.headers as Record<string, string> | undefined),
      };
      if (token) {
        headers.Authorization = `Bearer ${token}`;
      }
      return fetch(path, { ...init, headers });
    },
    [token],
  );
}
