export type View = "dashboard" | "discover" | "requests" | "series" | "movies" | "add" | "settings";
export type MediaType = "movie" | "series";
export type SubjectType = "episode" | "movie" | "grab";
export type SeriesMonitorMode = "future_only" | "all" | "latest_season" | "none";
export type RequestMonitorMode = "latest_season" | "all" | "future_only";
export type SeriesStatus = "following" | "paused" | "ended";
export type TrialScope = "one_episode" | "three_episodes" | "first_season";
export type TrialDecision = "continue" | "stop";
export type RecommendationAction = "dismiss" | "request" | "rate";

export interface ItemList<T> {
  items?: T[] | null;
}

export interface HealthComponent {
  name: string;
  kind: string;
  status: string;
  critical: boolean;
  detail?: string;
}

export interface HealthResponse {
  status: string;
  components?: HealthComponent[] | null;
}

export interface Grab {
  id: number;
  subject_type: SubjectType;
  subject_id: number;
  release_id: number;
  state: string;
  progress: number;
  content_path?: string;
  updated_at: string;
}

export interface QueueResponse extends ItemList<Grab> {
  paused: boolean;
}

export interface HistoryResponse extends ItemList<Grab> {
  page: number;
}

export interface Movie {
  id: number;
  title: string;
  year: number;
  state: string;
  quality_profile_id: number;
  imported_path?: string;
  imported_quality?: string;
}

export interface QualityProfile {
  id: number;
  name: string;
  is_default: boolean;
  allowed_resolutions: string[];
  allowed_sources: string[];
  min_seeders: number;
}

export interface Series {
  id: number;
  title: string;
  year?: number;
  monitor_mode: SeriesMonitorMode;
  status: SeriesStatus;
  quality_profile_id: number;
}

export interface SeriesPatch {
  profile_id?: number;
  monitor_mode?: SeriesMonitorMode;
  status?: SeriesStatus;
}

export interface Episode {
  id: number;
  season: number;
  number: number;
  title?: string;
  air_date?: string | null;
  state: string;
  imported_path?: string;
}

export interface SeriesDetailResponse {
  series: Series;
  episodes?: Episode[] | null;
}

export interface JellyfinUser {
  server_id: string;
  user_id: string;
  display_name: string;
  enabled: boolean;
  last_synced_at?: string;
}

export interface Recommendation {
  id: number;
  title: string;
  year?: number;
  overview?: string;
  poster_url?: string;
  score: number;
  vote_average: number;
  vote_count: number;
  genres?: string[];
  runtime_minutes?: number;
  reasons?: string[];
  tmdb_id: number;
  generated_at: string;
  expires_at: string;
}

export interface RecommendationRating {
  tmdb_id: number;
  rating: number;
}

export interface RecommendationHistoryResponse {
  items: Recommendation[];
  ratings: RecommendationRating[];
}

export interface RecommendationPreferences {
  languages: string[];
  excluded_genres: string[];
  familiarity: string;
  diversity: number;
}

export interface PreviewVideo {
  name: string;
  key: string;
  type: string;
  official: boolean;
}

export interface RecommendationPreview {
  title: string;
  year: number;
  media_type: MediaType;
  overview: string;
  poster_url: string;
  genres: string[];
  people: string[];
  runtime_minutes: number;
  vote_average: number;
  vote_count: number;
  videos: PreviewVideo[];
  seasons?: number[];
}

export interface Trial {
  request_id: number;
  title: string;
  scope: TrialScope;
  episode_limit: number;
  watched_episodes: number;
  state: string;
  decision: string;
  rating: number;
}

export interface MediaRequest {
  id: number;
  title: string;
  year: number;
  media_type: MediaType;
  requested_at: string;
  monitor_mode: RequestMonitorMode | "";
  state: string;
  progress: number;
  last_error: string | null;
  next_search_at: string | null;
  available: boolean;
  imported_episodes: number;
  total_episodes: number;
  cancelled_at: string | null;
  seasons: number[] | null;
  jellyfin_url: string;
  trial?: Trial;
}

export interface RequestListResponse {
  items: MediaRequest[];
  has_more: boolean;
}

export interface StateTransition {
  reason: string;
  detail?: string;
  at: string;
}

export interface DiagnosticCandidate {
  evaluation: { accepted: boolean; reason: string; score: number };
  release: { id: number; raw_title: string };
}

export interface DiagnosticSubject {
  type: SubjectType;
  id: number;
  title: string;
  state: string;
  error: string;
  search_attempts: number;
  next_search_at?: string;
  last_search_at?: string;
  history?: StateTransition[] | null;
  candidates: DiagnosticCandidate[];
}

export interface DiagnosticsResponse {
  subjects: DiagnosticSubject[];
}

export interface MetadataMatch {
  title: string;
  year: number;
  overview?: string;
  poster_url?: string;
  tmdb_id?: number;
  tvmaze_id?: number;
}

export interface MetadataSearchResponse {
  items?: MetadataMatch[] | null;
}

export interface SettingsIndexer {
  name: string;
  base_url: string;
  enabled: boolean;
}

export interface SettingsResponse {
  server: { bind: string; port: number; auth_enabled: boolean };
  indexers?: SettingsIndexer[] | null;
  downloader: { type: string; url: string };
  library: { TVRoot?: string; MovieRoot?: string; tv_root?: string; movie_root?: string };
  recommendations: { enabled: boolean; refresh_interval: string; result_limit: number };
}

export interface SetupCheck {
  name: string;
  status: string;
  detail?: string;
  action: string;
  available_bytes?: number;
}

export interface SetupResponse {
  checks: SetupCheck[];
  webhook_enabled: boolean;
}

export interface DialogCheck {
  id: string;
  label: string;
  detail: string;
  checked?: boolean;
  required?: boolean;
}

export interface ConfirmOptions {
  title: string;
  message: string;
  confirmLabel: string;
  checks: DialogCheck[];
}
