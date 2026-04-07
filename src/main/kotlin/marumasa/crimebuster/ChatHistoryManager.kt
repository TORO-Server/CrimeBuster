package marumasa.crimebuster

import java.util.*
import java.util.concurrent.ConcurrentLinkedQueue

class ChatHistoryManager(private val maxHistory: Int = 20) {
    private val history: Queue<String> = ConcurrentLinkedQueue<String>()

    fun addMessage(playerName: String, message: String) {
        val entry = "[$playerName] $message"
        history.add(entry)
        while (history.size > maxHistory) {
            history.poll()
        }
    }

    fun getHistory(): List<String> {
        return history.toList()
    }

    fun getHistoryAsString(): String {
        return history.joinToString("\n")
    }
}
