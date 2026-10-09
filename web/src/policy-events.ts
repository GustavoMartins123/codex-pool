const policySavedEvent = "pool:policy-saved";
type PolicySaved = { principalID: string; editorID: string };

export function notifyPolicySaved(value: PolicySaved) {
  window.dispatchEvent(new CustomEvent<PolicySaved>(policySavedEvent, { detail: value }));
}

export function subscribePolicySaved(listener: (value: PolicySaved) => void) {
  const handle = (event: Event) => listener((event as CustomEvent<PolicySaved>).detail);
  window.addEventListener(policySavedEvent, handle);
  return () => window.removeEventListener(policySavedEvent, handle);
}
