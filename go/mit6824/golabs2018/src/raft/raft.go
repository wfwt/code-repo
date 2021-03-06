package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import "sync"
import "labrpc"
import "time"
import "math/rand"
import "fmt"
import "bytes"
import "labgob"
import "strconv"

//
// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make(). set
// CommandValid to true to indicate that the ApplyMsg contains a newly
// committed log entry.
//
// in Lab 3 you'll want to send other kinds of messages (e.g.,
// snapshots) on the applyCh; at that point you can add fields to
// ApplyMsg, but set CommandValid to false for these other uses.
//
type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int
	CommandTerm  int
}

// 3种状态
const STATE_LEADER int = 1
const STATE_FOLLOWER int = 2
const STATE_CANDIDATE int = 3
const STATE_KILLED int = -1 // 表示raft终止了

// 无效的服务器id
const NULL_PEER_ID int = -1

// 无效的任期
const NULL_TERM = 0
// 第一个日志索引
const FIRST_LOG_INDEX = 1
// 无效的日志索引
const NULL_LOG_INDEX = FIRST_LOG_INDEX - 1

// 超时时间
const HEARTBEAT_TIMEOUT = 100
const MIN_ELECTION_TIMEOUT = 500
const MAX_ELECTION_TIMEOUT = 1000
const ELECTION_TIMER_CHECK_SLEEP = 10
const HEARTBEAT_CHECK_SLEEP = 10

// 是否打开Raft调试
const DEBUG_RAFT = false
const DEBUG_RAFT_LOCK = false
const DEBUG_RAFT_PERSIST = false
const DEBUG_RAFT_LEADER = false

type LogEntry struct {
	Term  int // Leader的当前任期
	Index int // Leader给日志分配的索引

	Command interface{} // 日志命令
}

//
// A Go object implementing a single Raft peer.
//
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]

	// Your data here (2A, 2B, 2C).
	// Look at the paper's Figure 2 for a description of what
	// state a Raft server must maintain.

	// 当收到客户端请求时，通知日志复制处理线程
	logReplicationConds []*sync.Cond
	// commitIndex更新的信号量
	commitIndexCond *sync.Cond

	lastSendApplyChIndex int // 类似lastApplied，发送给ApplyCh的日志索引，默认为0
	
	lastLeaderUpdateTimestamp time.Time // 最新的Leader更新时间戳

	electionTimestamp time.Time // 选举超时检测的时间
	electionTimeout time.Duration // 选举超时时间

	lastHeartbeatTimestamp time.Time // 上一次发送心跳的时间戳
	
	state int 	// 当前状态
	
	currentTerm int 	// 当前任期，默认值为0
	votedFor int 	// 当前任期之内投票的候选人id，默认为空
	log []LogEntry  // 日志，第一条日志索引为1

	commitIndex int // 已经提交的最高的日志索引，默认值为0
	lastApplied int // 已经应用到状态机的最高的日志索引，默认为0

	// Leader特有的信息 ----------------------------
	nextIndex []int // 对于每一个服务器，下一条发送日志索引，初始化为Leader的最新日志索引+1
	matchIndex [] int // 对于每一个服务器，最高的commitIndex，初始化为0

	
	// -------------------------------------------
}

func stateToString(state int) string {
	switch state {
	case STATE_FOLLOWER:
		return "Follower"
	case STATE_CANDIDATE:
		return "Candidate"
	case STATE_LEADER:
		return "Leader"
	case STATE_KILLED :
		return "Killed"
	default:
		return fmt.Sprintf("其他%d", state)
	}
}
func (rf *Raft)formatLogInfo() string {
	logStr := ""
	logStr += "["
	
	for i := 0; i < len(rf.log); i++ {
		if i != 0 {
			logStr += "," + strconv.Itoa(rf.log[i].Term)
		} else {
			logStr += "" + strconv.Itoa(rf.log[i].Term)
		}
	}
	logStr += "]"

	return logStr
}

func (rf *Raft)formatStatusInfo(me int, currentTerm int, state int) string {
	now := time.Now()
	
	return fmt.Sprintf("%d-%d-%d %d:%d:%d.%d Raft[%d]任期[%d]状态[%s] ",
		now.Year(), now.Month(), now.Day(),
		now.Hour(), now.Minute(), now.Second(), now.Nanosecond()/1000000,
		me, currentTerm, stateToString(state))
}
func (rf *Raft) getStatusInfo() string {
	return rf.formatStatusInfo(rf.me, rf.currentTerm, rf.state)
}
func (rf *Raft) getStatusInfoByLock() string {
	rf.Lock()
	defer rf.Unlock()
	
	currentTerm := rf.currentTerm
	state := rf.state
	return rf.formatStatusInfo(rf.me, currentTerm, state)
}

func (rf *Raft)Lock() {
	if DEBUG_RAFT_LOCK {
		mylog(rf.getStatusInfo(), "开始Lock")
	}
	rf.mu.Lock()
	if DEBUG_RAFT_LOCK {
		mylog(rf.getStatusInfo(), "得到Lock")
	}
}
func (rf *Raft)Unlock() {
	if DEBUG_RAFT_LOCK {
		mylog(rf.getStatusInfo(), "Unlock")
	}
	rf.mu.Unlock()
}

func (rf *Raft)getMajorityNum() int {
	return len(rf.peers)/2 + 1
}

func (rf *Raft)getLastLogTermAndIndex() (int, int) {
	logSize := len(rf.log)

	if logSize > 0 {
		return rf.log[logSize - 1].Term, rf.log[logSize - 1].Index
	} else {
		return NULL_TERM, FIRST_LOG_INDEX - 1
	}
}
func (rf *Raft)getLastLogIndex() int {
	_, i := rf.getLastLogTermAndIndex()
	return i
}

func (rf *Raft)logIndexToLogArrayIndex(v int) int {
	logSize := len(rf.log)

	if logSize > 0 {
		return v - rf.log[0].Index
	} else {
		return -1
	}	
}

func (rf *Raft)getLogTermByLogIndex(v int) int {
	logSize := len(rf.log)

	if logSize > 0 {
		arrayIndex := rf.logIndexToLogArrayIndex(v)

		if arrayIndex >= 0 {
			return rf.log[arrayIndex].Term
		} else {
			return NULL_TERM	
		}
	} else {
		return NULL_TERM
	}
}

func (rf *Raft)getFirstLogIndexByTermForward(endArrayIndex int, term int) int {
	logIndex := NULL_LOG_INDEX
	
	for i := endArrayIndex; i >= 0; i--{
		if rf.log[i].Term != term {
			break
		} else {
			logIndex = rf.log[i].Index
		}
	}

	return logIndex
}

// 从nextIndex往前找
func (rf *Raft)optimizeNextIndex(nextIndex int, conflictTerm int, conflictIndex int) int {
	arrayIndex := rf.logIndexToLogArrayIndex(nextIndex)

	if arrayIndex >= 0 {
		i := arrayIndex
		k := -1
		
		for ; i >= 0; i-- {
			if rf.log[i].Term == conflictTerm {
				k = i
				break
			}
		}

		if k >= 0 {
			return rf.log[k + 1].Index
		} else {
			return conflictIndex
		}
	} else {
		return NULL_LOG_INDEX
	}
}

// 比较自己的日志和对方的日志，返回1表示自己的更新；返回0表示一样新；返回-1表示自己的更旧；
func (rf *Raft)checkLogUpToDate(logTerm int, logIndex int) int {
	n := len(rf.log)
	if n > 0 {
		if rf.log[n - 1].Term > logTerm {
			return 1
		} else if rf.log[n - 1].Term < logTerm {
			return -1
		} else {
			if rf.log[n - 1].Index > logIndex {
				return 1
			} else if rf.log[n - 1].Index < logIndex {
				return -1
			} else {
				return 0
			}
		}
	} else {
		return -1
	}
}

func (rf *Raft) voteForCandidate(args *RequestVoteArgs) bool {
	if (rf.votedFor == NULL_PEER_ID || rf.votedFor == args.CandidateId) && (rf.checkLogUpToDate(args.LastLogTerm, args.LastLogIndex) <= 0) {
		rf.votedFor = args.CandidateId
		rf.persist()
		// 给其他候选人投票了，重置选举超时
		mylog(rf.getStatusInfo(), "给Raft[", args.CandidateId, "]投票，因此重置选举超时")
		rf.resetElectionTimerForVote()
		mylog(rf.getStatusInfo(), "新选举超时时间", rf.electionTimeout)
		
		return true
	} else {
		return false
	}
}
func (rf *Raft)resetElectionTimerForVote() {
	rf.resetElectionTimer()
}
func (rf *Raft)resetElectionTimerForLeader() {
	rf.lastLeaderUpdateTimestamp = time.Now()
	rf.electionTimestamp = rf.lastLeaderUpdateTimestamp
	rf.electionTimeout = rf.getElectionTimeout()
}
func (rf *Raft)resetElectionTimer() {
	rf.electionTimestamp = time.Now()
	rf.electionTimeout = rf.getElectionTimeout()
}
func (rf *Raft)isElectionTimeout() bool {
	if time.Since(rf.electionTimestamp) > rf.electionTimeout {
		return true
	} else {
		return false
	}
}
func (rf *Raft)isHeartbeatTimeout() bool {
	if time.Since(rf.lastHeartbeatTimestamp) > rf.getHeartbeatTimeout() {
		return true
	} else {
		return false
	}
}

// 获取随机的选举超时时间，单位毫秒
func (rf *Raft) getElectionTimeout() time.Duration {
	ms := MIN_ELECTION_TIMEOUT + (rand.Int63() % (MAX_ELECTION_TIMEOUT - MIN_ELECTION_TIMEOUT))
	return time.Duration(ms) * time.Millisecond
}

// 获取Leader发送心跳信息的时间，单位毫秒
func (rf *Raft) getHeartbeatTimeout() time.Duration {
	ms := HEARTBEAT_TIMEOUT
	return time.Duration(ms) * time.Millisecond
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {

	var term int
	var isleader bool
	// Your code here (2A).

	rf.Lock()
	term = rf.currentTerm
	isleader = (rf.state == STATE_LEADER)
	rf.Unlock()
	
	return term, isleader
}

//
// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart
// see paper's Figure 2 for a description of what should be persistent.
//
func (rf *Raft) persist() {
	// Your code here (2C).
	// Example:
	// w := new(bytes.Buffer)
	// e := labgob.NewEncoder(w)
	// e.Encode(rf.xxx)
	// e.Encode(rf.yyy)
	// data := w.Bytes()
	// rf.persister.SaveRaftState(data)

	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)

	e.Encode(rf.currentTerm)
	e.Encode(rf.votedFor)
	e.Encode(rf.log)
	
	data := w.Bytes()
	rf.persister.SaveRaftState(data)

	if DEBUG_RAFT_PERSIST {
		fmt.Println(rf.getStatusInfo(), "日志:", rf.formatLogInfo())
	}
}

//
// restore previously persisted state.
//
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}
	// Your code here (2C).
	// Example:
	// r := bytes.NewBuffer(data)
	// d := labgob.NewDecoder(r)
	// var xxx
	// var yyy
	// if d.Decode(&xxx) != nil ||
	//    d.Decode(&yyy) != nil {
	//   error...
	// } else {
	//   rf.xxx = xxx
	//   rf.yyy = yyy
	// }

	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)

	var currentTerm int
	var votedFor int
	var log []LogEntry

	if d.Decode(&currentTerm) != nil || d.Decode(&votedFor) != nil || d.Decode(&log) != nil {
		// 读取出错
	} else {
		rf.currentTerm = currentTerm
		rf.votedFor = votedFor
		rf.log = log
	}
}

//
// example RequestVote RPC arguments structure.
// field names must start with capital letters!
//
type RequestVoteArgs struct {
	// Your data here (2A, 2B).
	Term         int // 候选人的任期
	CandidateId  int // 候选人的id
	LastLogIndex int // 候选人最新日志的索引
	LastLogTerm  int // 候选人最新日志的任期
}

//
// example RequestVote RPC reply structure.
// field names must start with capital letters!
//
type RequestVoteReply struct {
	// Your data here (2A).

	Term        int  // 当前任期，用于候选人更新自己的任期
	VoteGranted bool // true表示收到了投票
}

//
// example RequestVote RPC handler.
//
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	if DEBUG_RAFT {
		rf.Lock()
		mydebug(rf.getStatusInfo(), "收到RequestVote RPC")
		rf.Unlock()
	}

	// Your code here (2A, 2B).

	// All Servers : If RPC request or response contains term T > currentTerm: set currentTerm = T, convert to follower
	// 1. Reply false if term < currentTerm (§5.1)
	// 2. If votedFor is null or candidateId, and candidate’s log is at least as up-to-date as receiver’s log, grant vote

	rf.Lock()
	
	if args.Term > rf.currentTerm {
		// 对方任期更高
		mylog(rf.getStatusInfo(), "RequestVote RPC发现更高Raft[", args.CandidateId, "]任期[", args.Term, "]，切换到Follower")
		rf.disconverHigherTerm(args.Term)
	}

	if args.Term < rf.currentTerm {
		// 对方任期更低
		reply.Term = rf.currentTerm
		reply.VoteGranted = false
	} else {
		// 任期相等
		reply.Term = rf.currentTerm
		reply.VoteGranted = rf.voteForCandidate(args)
	}

	rf.Unlock()
}

type AppendEntriesArgs struct {
	Term         int // Leader的任期
	LeaderId     int // Leader的id
	PrevLogIndex int // Entries[]前面的一个日志索引
	PrevLogTerm  int // Entries[]前面的一个日志任期
	Entries      []LogEntry
	LeaderCommit int // Leader的commitIndex
}

type AppendEntriesReply struct {
	Term    int  // 当前任期
	Success bool // true表示Follower匹配
	
	ConflictIndex int // 冲突的日志索引
	ConflictTerm int // 冲突的任期
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	if DEBUG_RAFT {
		rf.Lock()
		mydebug(rf.getStatusInfo(), "收到AppendEntries RPC")
		rf.Unlock()
	}
	
	// All Servers : If RPC request or response contains term T > currentTerm: set currentTerm = T, convert to follower
	// 1. Reply false if term < currentTerm (§5.1)
	// 2. Reply false if log doesn’t contain an entry at prevLogIndex
	// whose term matches prevLogTerm (§5.3)
	// 3. If an existing entry conflicts with a new one (same index
	// but different terms), delete the existing entry and all that
	// follow it (§5.3)
	// 4. Append any new entries not already in the log
	// 5. If leaderCommit > commitIndex, set commitIndex =
	// min(leaderCommit, index of last new entry)

	rf.Lock()
	defer rf.Unlock()
	
	if args.Term > rf.currentTerm {
		// 对方任期更高
		mylog(rf.getStatusInfo(), "AppendEntries RPC发现更高Raft[", args.LeaderId, "]任期[", args.Term, "]，切换到Follower")
		rf.disconverHigherTerm(args.Term)
	}

	if args.Term < rf.currentTerm {
		// 对方任期更低
		reply.Term = rf.currentTerm
		reply.Success = false
	} else {
		// 任期相等
		
		// 从当前Leader收到了一个AppendEntries RPC
		rf.resetElectionTimerForLeader()

		prevLogArrayIndex := rf.logIndexToLogArrayIndex(args.PrevLogIndex)
		if prevLogArrayIndex >= len(rf.log) { // 超了
			reply.Term = rf.currentTerm
			reply.Success = false
			reply.ConflictIndex = rf.getLastLogIndex() + 1
			reply.ConflictTerm = NULL_TERM
		} else if prevLogArrayIndex < 0 {
			// 说明日志不在rf.log中，在没有快照的情况下，说明这是不存在的日志
			reply.Term = rf.currentTerm
			reply.Success = true

			rf.tryAppendEntries(0, args)
		} else {
			if rf.log[prevLogArrayIndex].Term == args.PrevLogTerm {
				reply.Term = rf.currentTerm
				reply.Success = true

				rf.tryAppendEntries(prevLogArrayIndex + 1, args)
			} else {
				reply.Term = rf.currentTerm
				reply.Success = false
				reply.ConflictTerm = rf.log[prevLogArrayIndex].Term
				reply.ConflictIndex = rf.getFirstLogIndexByTermForward(prevLogArrayIndex, reply.ConflictTerm)
			}
		}
	}
}

func (rf *Raft)tryAppendEntries(startLogArrayIndex int, args *AppendEntriesArgs) {
	if len(args.Entries) > 0 {
		conflictLogArrayIndex := len(rf.log)

		i := 0
		k := startLogArrayIndex
	
		for ; i < len(args.Entries) && k < len(rf.log); i++ {
			if args.Entries[i].Term != rf.log[k].Term {
				conflictLogArrayIndex = k
				break
			}
			k++
		}


		logChange := false
		
		if conflictLogArrayIndex != len(rf.log) {
			rf.log = rf.log[0 : conflictLogArrayIndex]
			logChange = true
		}

		for ; i < len(args.Entries); i++ {
			mylog(rf.getStatusInfo(), "收到AppendEntries RPC, 日志为：", args.Entries[i])
			rf.log = append(rf.log, args.Entries[i])
			logChange = true
		}

		if logChange {
			rf.persist()
		}
	}

	if args.LeaderCommit > rf.commitIndex {
		lastLogIndex := rf.getLastLogIndex()

		if args.LeaderCommit <= lastLogIndex {
			rf.commitIndex = args.LeaderCommit
		} else {
			rf.commitIndex = lastLogIndex
		}

		mylog(rf.getStatusInfo(), "更新commitIndex：", rf.commitIndex)

		go func() {
			rf.commitIndexCond.L.Lock()
			rf.commitIndexCond.Broadcast()
			rf.commitIndexCond.L.Unlock()
		}()
	}
}

//
// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
//
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	index := -1
	term := -1
	isLeader := true

	// Your code here (2B).
	rf.Lock()
	if rf.state != STATE_LEADER {
		isLeader = false
	} else {
		// 领导人把这条命令作为新的日志条目加入到它的日志中去，
		// 然后通知日志复制线程

		newLogIndex := rf.getLastLogIndex() + 1
		
		newLogEntry := LogEntry{
			Term : rf.currentTerm,
			Index : newLogIndex,
			Command : command,
		}
		rf.log = append(rf.log, newLogEntry)
		rf.persist()

		mylog(rf.getStatusInfo(), "接收到新命令:", newLogEntry)
		
		index = newLogIndex
		term = rf.currentTerm

		go func() {
			for i := 0; i < len(rf.peers); i++ {
				rf.logReplicationConds[i].L.Lock()
				rf.logReplicationConds[i].Signal()
				rf.logReplicationConds[i].L.Unlock()
			}
		}()
	}
	rf.Unlock()

	return index, term, isLeader
}

//
// the tester calls Kill() when a Raft instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (rf *Raft) Kill() {
	// Your code here, if desired.

	rf.Lock()
	defer rf.Unlock()
	
	if DEBUG_RAFT {
		mylog(rf.getStatusInfo(), "Kill")
	}
	rf.state = STATE_KILLED

	go func() {
		for i := 0; i < len(rf.peers); i++ {
			rf.logReplicationConds[i].L.Lock()
			rf.logReplicationConds[i].Signal()
			rf.logReplicationConds[i].L.Unlock()
		}

		rf.commitIndexCond.L.Lock()
		rf.commitIndexCond.Broadcast()
		rf.commitIndexCond.L.Unlock()
	}()
}

//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me

	// Your initialization code here (2A, 2B, 2C).

	rf.logReplicationConds = make([]*sync.Cond, len(rf.peers))
	for i := 0; i < len(rf.peers); i++ {
		locker := new(sync.Mutex)
		rf.logReplicationConds[i] = sync.NewCond(locker)
	}
	rf.commitIndexCond = sync.NewCond(new(sync.Mutex))

	rf.lastSendApplyChIndex = 0
	
	rf.resetElectionTimer()
	rf.state = STATE_FOLLOWER
	rf.currentTerm = 0
	rf.votedFor = NULL_PEER_ID
	rf.commitIndex = 0
	rf.lastApplied = 0

	rf.nextIndex = make([]int, len(rf.peers))
	rf.matchIndex = make([]int, len(rf.peers))

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	// 一个线程用于定时检查选举是否超时
	go rf.handleElectionTimer()
	
	// 一个线程用于定时发送心跳
	go rf.handleHeartbeat()

	// 对于每一个服务器开启一个线程用于发送日志
	for i := 0; i < len(rf.peers); i++ {
		go rf.handleLogReplication(i)
	}
	
	// 一个线程用于把已经提交的日志发送到applyCh
	go rf.handleSendApplyCh(applyCh)
	
	// 一个线程用于apply
	go rf.handleLogApply()

	return rf
}

func (rf *Raft)handleElectionTimer() {
	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "开始选举超时检查线程")
	}

	for {
		rf.Lock()
		
		if rf.state == STATE_KILLED {
			rf.Unlock()
			break
		} else if rf.state == STATE_FOLLOWER && rf.isElectionTimeout() {
			// 转成候选人
			rf.convertToState(STATE_CANDIDATE)
		} else if rf.state == STATE_CANDIDATE && rf.isElectionTimeout() {
			// 发起新的选举
			rf.startNewElection()
		}
		
		rf.Unlock()
		
		time.Sleep(time.Duration(ELECTION_TIMER_CHECK_SLEEP) * time.Millisecond)
	}

	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "结束选举超时检查线程")
	}
}

// 转换到状态，当之前的状态和新状态一样时，不做任何处理
func (rf *Raft) convertToState(state int) {
	if rf.state != state {
		mylog(rf.getStatusInfo(), "转换状态到--->", stateToString(state))
		rf.internalStartNewState(state)
	} else {
		mylog(rf.getStatusInfo(), ",不用转换状态，因为状态一致：", stateToString(state))
	}
}

// 候选人发起新的选举
func (rf *Raft)startNewElection() {
	// • Increment currentTerm
	// • Vote for self
	// • Reset election timer
	// • Send RequestVote RPCs to all other servers

	rf.currentTerm++
	rf.votedFor = rf.me
	rf.persist()
	rf.resetElectionTimer()
	lastLogTerm, lastLogIndex := rf.getLastLogTermAndIndex()
	
	args := RequestVoteArgs{
		Term:         rf.currentTerm,
		CandidateId:  rf.votedFor,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	}
	var voteCount int  = 0

	// Send RequestVote RPCs to all other servers
	for i := 0; i < len(rf.peers); i++ {
		go func(server int) {
			reply := RequestVoteReply{}

			mydebug(rf.getStatusInfoByLock(), ",开始向服务器", server, "发送RequestVote RPC")
			ok := rf.sendRequestVote(server, &args, &reply)
			mydebug(rf.getStatusInfoByLock(), ",结束向服务器", server, "发送RequestVote RPC,", ok)

			if !ok {
				return
			}
			
			rf.Lock()
			defer rf.Unlock()

			if args.Term != rf.currentTerm {
				// 期间任期发生了变化，忽略
				mydebug(rf.getStatusInfo(), "发送投票期间从任期[", args.Term, "]变为任期[", rf.currentTerm, "]，忽略")
			} else {
				if reply.VoteGranted {
					voteCount++
					if voteCount == rf.getMajorityNum() {
						// 收到多数派投票
						if DEBUG_RAFT_LEADER {
							fmt.Println(rf.getStatusInfo(), "收到多数派投票：", voteCount, "/", len(rf.peers), "，VotedFor:", rf.votedFor, "，切换到", stateToString(STATE_LEADER))
						}
						rf.convertToState(STATE_LEADER)
					}
				} else {
					if rf.currentTerm < reply.Term {
						mylog(rf.getStatusInfo(), "AppendEntries RPC发现更高任期[", args.Term, "]，切换到Follower")
						rf.disconverHigherTerm(args.Term)
					}
				}
			}
		}(i)
	}
}


func (rf *Raft) internalStartNewState(state int) {
	rf.state = state

	if state == STATE_FOLLOWER {
		// 
	} else if state == STATE_CANDIDATE {
		rf.startNewElection()
	} else if state == STATE_LEADER {
		rf.startLeader()
	}
}

// 发现了更新任期
func (rf *Raft)disconverHigherTerm(higherTerm int) {
	rf.currentTerm = higherTerm
	rf.votedFor = NULL_PEER_ID
	rf.persist()

	if rf.state != STATE_FOLLOWER {
		mylog(rf.getStatusInfo(), "转换状态到--->", stateToString(STATE_FOLLOWER))
		rf.internalStartNewState(STATE_FOLLOWER)
	} else {
		mylog(rf.getStatusInfo(), "不用转换状态，因为状态一致：", stateToString(STATE_FOLLOWER))
	}
}

func (rf *Raft)startLeader() {
	lastLogIndex := rf.getLastLogIndex()
	
	for i := 0; i < len(rf.peers); i++ {
		rf.nextIndex[i] = lastLogIndex + 1
		rf.matchIndex[i] = 0
	}
}

func (rf *Raft)handleHeartbeat() {
	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "开始心跳检查线程")
	}
	
	for {
		rf.Lock()
	
		if rf.state == STATE_KILLED {
			rf.Unlock()
			break
		} else if rf.state == STATE_LEADER && rf.isHeartbeatTimeout() {
			// 心跳超时
			rf.sendHeartbeat()
		}

		rf.Unlock()
		
		time.Sleep(time.Duration(ELECTION_TIMER_CHECK_SLEEP) * time.Millisecond)
	}

	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "结束心跳检查线程")
	}
}

func (rf *Raft)sendHeartbeat() {
	for i := 0; i < len(rf.peers); i++ {
		if i != rf.me {
			go rf.sendHeartbeatToServer(i)
		}
	}

	rf.lastHeartbeatTimestamp = time.Now()
}

func (rf *Raft)handleLogReplication(server int) {
	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "开始日志复制线程,Server:", server)
	}

	rf.logReplicationConds[server].L.Lock()

	for {
		rf.logReplicationConds[server].Wait()

		rf.Lock()
		
		if rf.state == STATE_KILLED {
			rf.Unlock()
			break
		} else if rf.state == STATE_LEADER {
			if server != rf.me {
				rf.sendAppendEntriesRPC(server)
			}
		}
		
		rf.Unlock()
	}
		
	rf.logReplicationConds[server].L.Unlock()

	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "结束日志复制线程,Server:", server)
	}
}
func (rf *Raft)sendAppendEntriesRPC(server int) {
	// If last log index ≥ nextIndex for a follower: send AppendEntries RPC with log entries starting at nextIndex
	//   If successful: update nextIndex and matchIndex for follower (§5.3)
	//   If AppendEntries fails because of log inconsistency: decrement nextIndex and retry (§5.3)

	prevLogIndex := rf.nextIndex[server] - 1
	prevLogTerm := rf.getLogTermByLogIndex(prevLogIndex)
	entries := []LogEntry{}
	lastLogIndex := rf.getLastLogIndex()

	mydebug(rf.getStatusInfo(), "Server:", server, ",lastLogIndex:", lastLogIndex, ",nextIndex:", rf.nextIndex[server])
	if lastLogIndex >= rf.nextIndex[server] {
		startLogIndex := rf.nextIndex[server]
		endLogIndex := lastLogIndex
		startArrayIndex := rf.logIndexToLogArrayIndex(startLogIndex)
		endArrayIndex := rf.logIndexToLogArrayIndex(endLogIndex)
		
		entries = make([]LogEntry, endLogIndex - startLogIndex + 1)
		
		mylog(rf.getStatusInfo(), "需要发送Raft[", server, "]的日志索引从", startLogIndex, "到", endLogIndex, "，在log[]中的位置从", startArrayIndex, "到", endArrayIndex, ",log[]长度：", len(rf.log))
		
		for i := 0; startArrayIndex <= endArrayIndex; i++ {
			entries[i] = rf.log[startArrayIndex]
			startArrayIndex++
		}
	}
	
	args := AppendEntriesArgs{
		Term : rf.currentTerm,
		LeaderId : rf.me,
		PrevLogIndex : prevLogIndex, 
		PrevLogTerm : prevLogTerm,
		Entries : entries,
		LeaderCommit : rf.commitIndex,
	}

	go func() {
		reply := AppendEntriesReply{}
			
		mydebug("开始向服务器", server, "发送心跳信息AppendEntries RPC")
		ok := rf.sendAppendEntries(server, &args, &reply)
		mydebug("结束向服务器", server, "发送心跳信息AppendEntries RPC,", ok)

		rf.Lock()
		defer rf.Unlock()

		signal := false

		if ok {
			if len(args.Entries) > 0 {
				mylog(rf.getStatusInfo(), "收到Raft[", server, "]AppendEntriesReply:", reply)
			}
			
			if args.Term != rf.currentTerm {
				// 期间任期发生了变化，忽略
				mydebug(rf.getStatusInfo(), "发送日志期间从任期[", args.Term, "]变为任期[", rf.currentTerm, "]，忽略")
			} else if reply.Success {
				// 成功
				rf.nextIndex[server] = rf.getLastLogIndex() + 1
				rf.matchIndex[server] = args.PrevLogIndex + len(args.Entries)

				// 尝试更新Leader的commitIndex
				rf.tryUpdateLeaderCommitIndex()
			} else {
				// 失败
				if args.Term == reply.Term {
					signal = true
					optimizeIndex := rf.optimizeNextIndex(rf.nextIndex[server] - 1, reply.ConflictTerm, reply.ConflictIndex)
					//fmt.Println(rf.getStatusInfo(), "nextIndex:", rf.nextIndex[server], "optimizeIndex:", optimizeIndex, "，收到Raft[", server, "]，冲突索引:", reply.ConflictIndex, ",冲突任期", reply.ConflictTerm)
					//rf.nextIndex[server]--
					//rf.nextIndex[server] = reply.ConflictIndex
					rf.nextIndex[server] = optimizeIndex
				} else if rf.currentTerm < reply.Term {
					mylog(rf.getStatusInfo(), "AppendEntries RPC发现更高任期[", args.Term, "]，切换到Follower")
					rf.disconverHigherTerm(args.Term)
				} else {
					mylog(rf.getStatusInfo(), "收到比参数任期[", args.Term, "]更旧的任期[", reply.Term, "]，忽略")	
				}
			}
		}

		if signal {
			go func() {
				rf.logReplicationConds[server].L.Lock()
				rf.logReplicationConds[server].Signal()
				rf.logReplicationConds[server].L.Unlock()
			}()
		}
	}()
}
func (rf *Raft)tryUpdateLeaderCommitIndex() {
	// If there exists an N such that N > commitIndex, a majority
	// of matchIndex[i] ≥ N, and log[N].term == currentTerm: set commitIndex = N (§5.3, §5.4).

	lastLogIndex := rf.getLastLogIndex()
	n := rf.commitIndex + 1
	k := rf.logIndexToLogArrayIndex(n)
	maxN := NULL_LOG_INDEX

	for ; n <= lastLogIndex; n++ {
		count := 0
		
		for i := 0; i < len(rf.peers); i++ {
			if i != rf.me {
				if rf.matchIndex[i] >= n && rf.log[k].Term == rf.currentTerm {
					count++
				}
			} else {
				count++
			}
		}

		if count >= rf.getMajorityNum() {
			maxN = n
		}

		k++
	}

	if maxN != NULL_LOG_INDEX {
		mylog(rf.getStatusInfo(), "更新commitIndex：", maxN)
		rf.commitIndex = maxN

		go func() {
			rf.commitIndexCond.L.Lock()
			rf.commitIndexCond.Broadcast()
			rf.commitIndexCond.L.Unlock()
		}()
	}
}

func (rf *Raft)sendHeartbeatToServer(server int) {
	rf.Lock()
	defer rf.Unlock()

	if rf.state == STATE_LEADER {
		rf.sendAppendEntriesRPC(server)
	}
}

func (rf *Raft)handleSendApplyCh(applyCh chan ApplyMsg) {
	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "开始发送ApplyCh线程")
	}

	rf.commitIndexCond.L.Lock()

	for {
		// 解锁，并等待
		rf.commitIndexCond.Wait()
		
		rf.Lock()
		
		if rf.state == STATE_KILLED {
			rf.Unlock()
			break
		} else { // 不仅仅是Leader需要返回，其他的都需要返回
			i := rf.logIndexToLogArrayIndex(rf.lastSendApplyChIndex + 1)

			if i >= 0 {
				for ; i < len(rf.log); i++ {
					if rf.log[i].Index<= rf.commitIndex {
						msg := ApplyMsg{
							CommandValid : true,
							Command      : rf.log[i].Command,
							CommandIndex : rf.log[i].Index,
							CommandTerm  : rf.log[i].Term,
						}
						applyCh <- msg
						mylog(rf.getStatusInfo(), "向ApplyCh发送", msg)
						
						rf.lastSendApplyChIndex = rf.log[i].Index
					} else {
						break
					}
				}
			}
		}
		
		rf.Unlock()
	}
	
	rf.commitIndexCond.L.Unlock()

	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "结束发送ApplyCh线程")
	}
}

func (rf *Raft)handleLogApply() {
	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "开始日志Apply线程")
	}
	
	rf.commitIndexCond.L.Lock()

	for {
		// 解锁，并等待
		rf.commitIndexCond.Wait()
		
		rf.Lock()
		
		if rf.state == STATE_KILLED {
			rf.Unlock()
			break
		} else {
			i := rf.logIndexToLogArrayIndex(rf.lastApplied + 1)

			if i >= 0 {
				for ; i < len(rf.log); i++ {
					if rf.log[i].Index<= rf.commitIndex {
						mylog(rf.getStatusInfo(), "把任期[", rf.log[i].Term, "]索引[", rf.log[i].Index, "]的日志应用到状态机中")
						rf.lastApplied = rf.log[i].Index
					} else {
						break
					}
				}
			}
		}
		
		rf.Unlock()
	}
	
	rf.commitIndexCond.L.Unlock()

	if DEBUG_RAFT {
		mydebug(rf.getStatusInfoByLock(), "结束日志Apply线程")
	}
}
