import { useEffect, useState, useCallback } from 'react'
import { TimelinePage } from './pages/Timeline'
import { SearchPage } from './pages/Search'
import { SessionsPage } from './pages/Sessions'
import { SyncPage } from './pages/Sync'
import { SettingsPage } from './pages/Settings'
import { LogsPage } from './pages/Logs'
import { fetchProjects, fetchStats, getApiKey, setApiKey, clearApiKey, type ProjectInfo, type StatsResponse } from './api'
import './index.css'

type Page = 'timeline' | 'search' | 'sessions' | 'sync' | 'logs' | 'settings'

const tabs: { key: Page; label: string }[] = [
  { key: 'timeline', label: 'Timeline' },
  { key: 'search', label: 'Search' },
  { key: 'sessions', label: 'Sessions' },
  { key: 'sync', label: 'Sync' },
  { key: 'logs', label: 'Logs' },
  { key: 'settings', label: 'Settings' },
]

function LoginForm({ onLogin }: { onLogin: () => void }) {
  const [key, setKey] = useState('')
  const [error, setError] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setApiKey(key)
    try {
      const res = await fetch('/api/stats', { headers: { Authorization: `Bearer ${key}` } })
      if (res.status === 401) {
        setError(true)
        clearApiKey()
        return
      }
      onLogin()
    } catch {
      setError(true)
      clearApiKey()
    }
  }

  return (
    <div className="min-h-screen bg-gray-50 dark:bg-gray-900 flex items-center justify-center">
      <form onSubmit={handleSubmit} className="bg-white dark:bg-gray-800 p-8 rounded-lg shadow-md w-full max-w-sm">
        <h1 className="text-lg font-semibold mb-4 text-gray-900 dark:text-gray-100">agent-mem</h1>
        <label className="block text-sm text-gray-600 dark:text-gray-400 mb-2">API Key</label>
        <input
          type="password"
          value={key}
          onChange={(e) => { setKey(e.target.value); setError(false) }}
          placeholder="Enter API key"
          className="w-full px-3 py-2 border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 text-gray-900 dark:text-gray-100 focus:outline-none focus:ring-2 focus:ring-blue-500 mb-3"
          autoFocus
        />
        {error && <p className="text-red-500 text-sm mb-3">Invalid API key</p>}
        <button type="submit" className="w-full py-2 bg-blue-600 text-white rounded-md hover:bg-blue-700 transition-colors">
          Sign in
        </button>
      </form>
    </div>
  )
}

function App() {
  const [page, setPage] = useState<Page>('timeline')
  const [project, setProject] = useState('')
  const [projects, setProjects] = useState<ProjectInfo[]>([])
  const [stats, setStats] = useState<StatsResponse | null>(null)
  const [needsAuth, setNeedsAuth] = useState(false)
  const [authChecked, setAuthChecked] = useState(false)

  // Listen for 401 events from authFetch
  useEffect(() => {
    const handler = () => setNeedsAuth(true)
    window.addEventListener('agent-mem-unauthorized', handler)
    return () => window.removeEventListener('agent-mem-unauthorized', handler)
  }, [])

  // On mount, check if auth is needed
  useEffect(() => {
    fetch('/api/stats', { headers: getApiKey() ? { Authorization: `Bearer ${getApiKey()}` } : {} })
      .then((res) => {
        if (res.status === 401) setNeedsAuth(true)
        setAuthChecked(true)
      })
      .catch(() => setAuthChecked(true))
  }, [])

  const handleLogin = useCallback(() => {
    setNeedsAuth(false)
    loadData()
  }, [])

  const loadData = useCallback(() => {
    fetchProjects().then((data) => {
      setProjects(data || [])
      if (data?.length > 0 && !project) {
        setProject(data[0].name)
      }
    })
  }, [project])

  useEffect(() => {
    if (!needsAuth && authChecked) loadData()
  }, [needsAuth, authChecked])

  useEffect(() => {
    if (!needsAuth && authChecked) fetchStats(project || undefined).then(setStats)
  }, [project, needsAuth, authChecked])

  if (!authChecked) return null
  if (needsAuth) return <LoginForm onLogin={handleLogin} />

  return (
    <div className="min-h-screen bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-gray-100">
      <header className="bg-white dark:bg-gray-800 border-b border-gray-200 dark:border-gray-700 px-6 py-3">
        <div className="max-w-6xl mx-auto flex items-center justify-between">
          <div className="flex items-center gap-4">
            <h1 className="text-lg font-semibold">agent-mem</h1>
            {stats && (
              <span className="text-xs text-gray-400 font-mono">
                {stats.observations} obs, {stats.summaries} sum, {stats.prompts} prompts
              </span>
            )}
          </div>
          <div className="flex items-center gap-2">
            <select
              value={project}
              onChange={(e) => setProject(e.target.value)}
              className="px-3 py-1.5 text-sm border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 focus:outline-none focus:ring-2 focus:ring-blue-500"
            >
              <option value="">All Projects</option>
              {projects.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name} ({p.observation_count})
                </option>
              ))}
            </select>
          </div>
        </div>
      </header>

      <nav className="bg-white dark:bg-gray-800 border-b border-gray-200 dark:border-gray-700">
        <div className="max-w-6xl mx-auto flex gap-0">
          {tabs.map((tab) => (
            <button
              key={tab.key}
              onClick={() => setPage(tab.key)}
              className={`px-4 py-2.5 text-sm font-medium border-b-2 transition-colors ${
                page === tab.key
                  ? 'border-blue-500 text-blue-600 dark:text-blue-400'
                  : 'border-transparent text-gray-500 hover:text-gray-700 dark:hover:text-gray-300'
              }`}
            >
              {tab.label}
            </button>
          ))}
        </div>
      </nav>

      <main className="max-w-6xl mx-auto p-6">
        {page === 'timeline' && <TimelinePage project={project} />}
        {page === 'search' && <SearchPage project={project} />}
        {page === 'sessions' && <SessionsPage project={project} />}
        {page === 'sync' && <SyncPage />}
        {page === 'logs' && <LogsPage />}
        {page === 'settings' && <SettingsPage />}
      </main>
    </div>
  )
}

export default App
