// DTO types matching internal/admin/dto.go field names exactly.

export interface UserDTO {
  id: number;
  email: string;
  name: string;
  system_role: string;
  created_at: string; // RFC3339
}

export interface LoginResponse {
  user: UserDTO;
}

export interface MeResponse {
  user: UserDTO;
}

export interface ProtocolStat {
  protocol: string;
  bytes: number;
  objects: number;
  oldest_unix: number;
  newest_unix: number;
}

export interface StatsResponse {
  per_protocol: ProtocolStat[];
  total_bytes: number;
  total_objects: number;
  backend_disk_free: number;
  backend_disk_used: number;
}

export interface SeriesPoint {
  unix: number;
  bytes: number;
}

export interface SeriesResponse {
  protocol: string;
  points: SeriesPoint[];
}

export interface UpstreamHealth {
  protocol: string;
  url: string;
  blocked: boolean;
  last_err: string;
}

export interface UpstreamsResponse {
  upstreams: UpstreamHealth[];
}

export interface UsersResponse {
  users: UserDTO[];
  total: number;
}

export interface VerificationEvent {
  id: number;
  unix: number;
  protocol: string;
  artifact: string;
  digest: string;
  tier: string;
  result: 'pass' | 'fail' | 'warn';
  detail: string;
}

export interface EventsResponse {
  events: VerificationEvent[];
}

export interface UpstreamConfig {
  name: string;
  base_url: string;
  priority: number;
  official: boolean;
}

export interface ProtocolConfig {
  protocol: string;
  upstreams: UpstreamConfig[];
  verify_tiers: string[];
  mutable_ttl_seconds: number;
}

export interface ConfigResponse {
  data_plane_addr: string;
  control_plane_addr: string;
  blob_driver: string;
  meta_driver: string;
  protocols: ProtocolConfig[];
}

export interface CreateUserRequest {
  email: string;
  name: string;
  password: string;
  system_role: string;
}

export interface PatchUserRequest {
  name?: string;
  system_role?: string;
  password?: string;
}
