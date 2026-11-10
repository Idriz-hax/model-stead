export type SessionUser = { id: string; username: string; display_name?: string; role: string; must_change_password?: boolean };
export type Session = { csrf_token: string; user: SessionUser };
export type SetupInput = {code:string;username:string;display_name:string;password:string;password_confirmation:string};
export type PersonalUsage = {from:string;to:string;input_tokens:number|null;output_tokens:number|null;requests:number;incomplete_requests:number;days:{date:string;input_tokens:number|null;output_tokens:number|null;requests:number;incomplete:number}[]};
export type PersonalRequest = {request_id:string;endpoint:string;requested_model:string|null;enqueued_at:string;started_at:string|null;completed_at:string|null;outcome:string;response_status:number|null;input_tokens:number|null;output_tokens:number|null;total_tokens:number|null;usage_status:string};
export type RuntimeStatus = {enabled:boolean;status:'disabled'|'reachable'|'unauthorized'|'unavailable'|'unsupported'|'malformed';healthy:boolean;configured_models:string[];running_models:string[];sampled_at:string};
export type PersonalPage = {items:PersonalRequest[];limit:number;offset:number;total:number};
const origin = window.location.origin;
let csrf = '';
async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const mutation = !['GET', 'HEAD'].includes(init.method || 'GET');
  const headers = new Headers(init.headers);
  if (init.body) headers.set('Content-Type', 'application/json');
  if (mutation) { headers.set('Origin', origin); if (csrf) headers.set('X-CSRF-Token', csrf); }
  const controller = path === '/api/v1/admin/runtime' ? new AbortController() : undefined;
  const timeout = controller ? window.setTimeout(() => controller.abort(), 4000) : undefined;
  let response: Response;
  try { response = await fetch(path, { ...init, ...(controller ? {signal:controller.signal} : {}), headers, credentials: 'same-origin' }); }
  catch (e) { if (controller?.signal.aborted) throw new Error('Request timed out.'); throw e; }
  finally { if (timeout !== undefined) window.clearTimeout(timeout); }
  if (!response.ok) { let message = `${response.status} ${response.statusText}`; try { message = (await response.json()).error || message; } catch { /* use status */ } throw new Error(message); }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}
export const api = {
  async me() { const s = await request<Session>('/api/v1/auth/me'); csrf = s.csrf_token; return s; },
  setupState: () => request<{setup_required:boolean}>('/api/v1/auth/setup-state'),
  setupAdmin: (code:string,username:string,display_name:string,password:string,password_confirmation:string) => request<{ok:boolean}>('/api/v1/auth/setup',{method:'POST',body:JSON.stringify({code,username,display_name,password,password_confirmation} satisfies SetupInput)}),
  async login(username: string, password: string) { const s = await request<{csrf_token:string}>('/api/v1/auth/login', {method:'POST', headers:{Origin:origin}, body:JSON.stringify({username,password})}); csrf=s.csrf_token; return this.me(); },
  changePassword: (old_password:string,new_password:string) => request<{ok:boolean;user:{role:string;status:string};must_change_password:boolean}>('/api/v1/auth/change-password',{method:'POST',body:JSON.stringify({old_password,new_password})}),
  logout: () => request('/api/v1/auth/logout',{method:'POST'}),
  get: <T,>(path:string) => request<T>(`/api/v1/admin/${path}`),
  runtime: () => request<RuntimeStatus>('/api/v1/admin/runtime'),
  query: <T,>(path:string,params:Record<string,string|number|undefined>) => { const query=new URLSearchParams(); for(const [key,value] of Object.entries(params)) if(value!==undefined&&value!=='') query.set(key,String(value)); return request<T>(`/api/v1/admin/${path}?${query}`); },
  post: <T,>(path:string,body?:unknown) => request<T>(`/api/v1/admin/${path}`,{method:'POST',body:body===undefined?undefined:JSON.stringify(body)}),
  put: <T,>(path:string,body:unknown) => request<T>(`/api/v1/admin/${path}`,{method:'PUT',body:JSON.stringify(body)}),
  patch: <T,>(path:string,body:unknown) => request<T>(`/api/v1/admin/${path}`,{method:'PATCH',body:JSON.stringify(body)}),
  personalUsage: (params:Record<string,string|number|undefined>={}) => {const q=new URLSearchParams();for(const [k,v] of Object.entries(params))if(v!==undefined)q.set(k,String(v));return request<PersonalUsage>(`/api/v1/me/usage?${q}`)},
  personalRequests: (params:Record<string,string|number|undefined>={}) => {const q=new URLSearchParams();for(const [k,v] of Object.entries(params))if(v!==undefined&&v!=='')q.set(k,String(v));return request<PersonalPage>(`/api/v1/me/requests?${q}`)},
  cancelPersonalRequest: (id:string) => request<{ok:boolean}>(`/api/v1/me/requests/${encodeURIComponent(id)}/cancel`,{method:'POST'}),
  setCSRF(value:string) {csrf=value;},
};
