// Package db defines the R1 persistence models and their versioned GORM migrations.
package db

type SchemaMigration struct {
	Version int64 `gorm:"primaryKey;autoIncrement:false;check:version > 0"`
}

type Definition struct {
	ProcessKey      string     `gorm:"primaryKey;not null"`
	Version         int64      `gorm:"primaryKey;autoIncrement:false;not null;check:version > 0"`
	ProfileRevision string     `gorm:"not null"`
	Sources         JSONArray  `gorm:"not null"`
	Plan            JSONObject `gorm:"not null"`
	CreatedAt       int64      `gorm:"not null;autoCreateTime:false"`
}

type DefinitionHead struct {
	ProcessKey string      `gorm:"primaryKey;not null"`
	Version    int64       `gorm:"not null"`
	Definition *Definition `gorm:"belongsTo:Definition;foreignKey:ProcessKey,Version;references:ProcessKey,Version"`
}

type Operation struct {
	OperationID    string     `gorm:"primaryKey;not null"`
	IdempotencyKey CommandKey `gorm:"unique;not null"`
	Kind           string     `gorm:"not null;check:kind IN ('Deploy','Start','CompleteTask')"`
	Target         JSONObject `gorm:"not null"`
	Payload        JSONObject `gorm:"not null"`
	Status         string     `gorm:"not null;default:PENDING;check:outcome_shape,(status = 'PENDING' AND result IS NULL AND error IS NULL) OR (status = 'SUCCEEDED' AND result IS NOT NULL AND error IS NULL) OR (status = 'FAILED' AND result IS NULL AND error IS NOT NULL)"`
	Result         *JSONObject
	Error          *JSONObject
}

type Instance struct {
	InstanceID string      `gorm:"primaryKey;not null"`
	ProcessKey string      `gorm:"not null;index:instances_by_pin"`
	Version    int64       `gorm:"not null;index:instances_by_pin"`
	Status     string      `gorm:"not null;default:RUNNING;index:instances_by_pin;check:status IN ('RUNNING','INCIDENT','COMPLETED')"`
	Revision   int64       `gorm:"not null;default:1;check:revision > 0"`
	Variables  JSONObject  `gorm:"not null"`
	CreatedAt  int64       `gorm:"not null;autoCreateTime:false"`
	EndedAt    *int64      `gorm:"check:(status = 'COMPLETED') = (ended_at IS NOT NULL)"`
	Definition *Definition `gorm:"belongsTo:Definition;foreignKey:ProcessKey,Version;references:ProcessKey,Version"`
}

type Transition struct {
	InstanceID string `gorm:"primaryKey;not null"`
	Revision   int64  `gorm:"primaryKey;autoIncrement:false;not null;check:revision > 0 AND ((revision = 1) = (kind = 'STARTED'))"`
	Kind       string `gorm:"not null;check:kind IN ('STARTED','STEP','INCIDENT_RAISED','COMPLETED')"`
	ElementID  *string
	AtMS       int64     `gorm:"column:at_ms;not null"`
	Instance   *Instance `gorm:"belongsTo:Instance;foreignKey:InstanceID;references:InstanceID"`
}

type Activation struct {
	ActivationID string    `gorm:"primaryKey;not null;uniqueIndex:instance_activation,priority:2"`
	InstanceID   string    `gorm:"not null;uniqueIndex:instance_activation,priority:1;uniqueIndex:live_token_position,where:state <> 'CONSUMED'"`
	TokenID      string    `gorm:"not null;uniqueIndex:live_token_position"`
	ElementID    string    `gorm:"not null"`
	State        string    `gorm:"not null;check:state IN ('READY','WAITING','CONSUMED')"`
	Instance     *Instance `gorm:"belongsTo:Instance;foreignKey:InstanceID;references:InstanceID"`
}

type WorkItem struct {
	WorkID       string      `gorm:"primaryKey;not null;uniqueIndex:instance_work,priority:2;index:ready_work,priority:3;index:expiring_claims,priority:3"`
	Kind         string      `gorm:"not null;uniqueIndex:work_by_visit;index:ready_work,priority:1,where:state = 'READY';index:expiring_claims,priority:1,where:state = 'LEASED';check:kind IN ('COMMAND','STEP','TIMER')"`
	OperationID  *string     `gorm:"unique;check:work_cause,(kind = 'COMMAND' AND operation_id IS NOT NULL AND instance_id IS NULL AND activation_id IS NULL) OR (kind <> 'COMMAND' AND operation_id IS NULL AND instance_id IS NOT NULL AND activation_id IS NOT NULL)"`
	InstanceID   *string     `gorm:"uniqueIndex:instance_work,priority:1"`
	ActivationID *string     `gorm:"uniqueIndex:work_by_visit"`
	ReadyAt      int64       `gorm:"not null;index:ready_work,priority:2"`
	State        string      `gorm:"not null;default:READY;check:state IN ('READY','LEASED','BLOCKED','DONE')"`
	Owner        *string     `gorm:"check:claim_shape,(state = 'LEASED' AND owner IS NOT NULL AND lease_until IS NOT NULL AND fence > 0) OR (state <> 'LEASED' AND owner IS NULL AND lease_until IS NULL)"`
	Fence        int64       `gorm:"not null;default:0;check:fence >= 0"`
	LeaseUntil   *int64      `gorm:"index:expiring_claims,priority:2"`
	Operation    *Operation  `gorm:"belongsTo:Operation;foreignKey:OperationID;references:OperationID"`
	Activation   *Activation `gorm:"belongsTo:Activation;foreignKey:InstanceID,ActivationID;references:InstanceID,ActivationID"`
}

type Incident struct {
	InstanceID string      `gorm:"primaryKey;not null"`
	Revision   int64       `gorm:"primaryKey;autoIncrement:false;not null"`
	WorkID     string      `gorm:"not null"`
	ElementID  string      `gorm:"not null"`
	Code       string      `gorm:"not null"`
	Message    string      `gorm:"not null"`
	RetryInput JSONObject  `gorm:"not null"`
	Transition *Transition `gorm:"belongsTo:Transition;foreignKey:InstanceID,Revision;references:InstanceID,Revision"`
	Work       *WorkItem   `gorm:"belongsTo:WorkItem;foreignKey:InstanceID,WorkID;references:InstanceID,WorkID"`
}

type UserTask struct {
	TaskID       string    `gorm:"primaryKey;not null;index:tasks_by_status,priority:3"`
	ActivationID string    `gorm:"not null;unique"`
	FormID       string    `gorm:"not null"`
	Components   JSONArray `gorm:"not null"`
	CreatedAt    int64     `gorm:"not null;autoCreateTime:false;index:tasks_by_status,priority:2"`
	Status       string    `gorm:"not null;default:OPEN;index:tasks_by_status,priority:1;check:task_state,(status = 'OPEN' AND closed_at IS NULL AND reason IS NULL) OR (status = 'COMPLETED' AND closed_at IS NOT NULL AND reason IS NULL) OR (status = 'CANCELLED' AND closed_at IS NOT NULL AND reason IS NOT NULL AND reason = 'TIMER')"`
	ClosedAt     *int64
	Reason       *string
	Activation   *Activation `gorm:"belongsTo:Activation;foreignKey:ActivationID;references:ActivationID"`
}

type BoundaryTimer struct {
	TimerID      string    `gorm:"primaryKey;not null;index:armed_deadlines,priority:2"`
	ActivationID string    `gorm:"not null;unique"`
	ElementID    string    `gorm:"not null"`
	DueAt        int64     `gorm:"not null;index:armed_deadlines,priority:1,where:state = 'ARMED'"`
	State        string    `gorm:"not null;default:ARMED;check:state IN ('ARMED','FIRED','CANCELLED')"`
	Task         *UserTask `gorm:"belongsTo:UserTask;foreignKey:ActivationID;references:ActivationID"`
}

type Fork struct {
	ForkID            string      `gorm:"primaryKey;not null;uniqueIndex:instance_fork,priority:2"`
	InstanceID        string      `gorm:"not null;uniqueIndex:instance_fork,priority:1"`
	SplitActivationID string      `gorm:"not null;unique"`
	JoinElementID     string      `gorm:"not null"`
	State             string      `gorm:"not null;default:OPEN;check:state IN ('OPEN','CLOSED')"`
	Split             *Activation `gorm:"belongsTo:Activation;foreignKey:InstanceID,SplitActivationID;references:InstanceID,ActivationID"`
}

type BranchActivation struct {
	ActivationID string      `gorm:"primaryKey;not null;uniqueIndex:fork_branch_visit,priority:3"`
	InstanceID   string      `gorm:"not null"`
	ForkID       string      `gorm:"not null;uniqueIndex:fork_branch_visit,priority:1"`
	BranchFlowID string      `gorm:"not null;uniqueIndex:fork_branch_visit,priority:2"`
	Activation   *Activation `gorm:"belongsTo:Activation;foreignKey:InstanceID,ActivationID;references:InstanceID,ActivationID"`
	Fork         *Fork       `gorm:"belongsTo:Fork;foreignKey:InstanceID,ForkID;references:InstanceID,ForkID"`
}

type JoinArrival struct {
	ForkID         string            `gorm:"primaryKey;not null;uniqueIndex:fork_branch_arrival"`
	IncomingFlowID string            `gorm:"primaryKey;not null"`
	BranchFlowID   string            `gorm:"not null;uniqueIndex:fork_branch_arrival"`
	ActivationID   string            `gorm:"not null"`
	Branch         *BranchActivation `gorm:"belongsTo:BranchActivation;foreignKey:ForkID,BranchFlowID,ActivationID;references:ForkID,BranchFlowID,ActivationID"`
}
