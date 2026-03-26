import { useEffect, useState } from 'react'
import { TimelinePage } from './pages/Timeline'
import { SearchPage } from './pages/Search'
import { SessionsPage } from './pages/Sessions'
import { SyncPage } from './pages/Sync'
import { SettingsPage } from './pages/Settings'
import { LogsPage } from './pages/Logs'
import { fetchProjects, fetchStats, type ProjectInfo, type StatsResponse } from './api'
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

function App() {
  const [page, setPage] = useState<Page>('timeline')
  const [project, setProject] = useState('')
  const [projects, setProjects] = useState<ProjectInfo[]>([])
  const [stats, setStats] = useState<StatsResponse | null>(null)

  useEffect(() => {
    fetchProjects().then((data) => {
      setProjects(data || [])
      if (data?.length > 0 && !project) {
        setProject(data[0].name)
      }
    })
  }, [])

  useEffect(() => {
    fetchStats(project || undefined).then(setStats)
  }, [project])

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
