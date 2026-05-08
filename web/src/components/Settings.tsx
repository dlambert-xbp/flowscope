// Settings is now a thin entrypoint around the multi-section shell.
// The substance lives in components/settings/Shell.tsx and the
// per-section files under components/settings/sections/. SNMP
// credential management — formerly the only thing on this page —
// is now one section among many.
export { SettingsShell as Settings } from './settings/Shell'
