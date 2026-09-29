import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('monitor', Path(__file__).with_name('monitor.py'))
monitor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(monitor)


class MonitorTests(unittest.TestCase):
    def test_core_queue_backlog_is_monitored(self):
        rows = '\n'.join(name+' 0 0' for name in ('owlet.events','owlet.retry','owlet.dead','owlet.core.action_changed','owlet.core.published','owlet.core.embedding'))
        self.assertTrue(monitor.queues_healthy(rows, True))
        self.assertFalse(monitor.queues_healthy(rows.replace('owlet.core.embedding 0 0','owlet.core.embedding 200 0'), True))
        self.assertFalse(monitor.queues_healthy('owlet.events 0 0\nowlet.retry 0 0\nowlet.dead 0 0', True))
    def test_failure_and_recovery_hysteresis(self):
        state = {}
        for i in range(3):
            state, events = monitor.advance(state, {'queue': False})
            self.assertEqual(events, [{'event': 'alert', 'check': 'queue'}] if i == 2 else [])
        state, events = monitor.advance(state, {'queue': False})
        self.assertEqual(events, [])
        for i in range(3):
            state, events = monitor.advance(state, {'queue': True})
            self.assertEqual(events, [{'event': 'recovered', 'check': 'queue'}] if i == 2 else [])
        self.assertFalse(state['queue']['active'])

    def test_interrupted_streak_does_not_alert(self):
        state = {}
        for ok in (False, False, True, False, False, True):
            state, events = monitor.advance(state, {'db': ok})
            self.assertEqual(events, [])


if __name__ == '__main__':
    unittest.main()
