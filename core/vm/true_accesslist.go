package vm

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state" // stateDB的AccessList
	"github.com/ethereum/go-ethereum/core/types" // 用户的AccessList

	// 追踪器的AccessList
	"github.com/ethereum/go-ethereum/params"
)

// JudgeAccessList 自定义AL类型
type JudgeAccessList struct {
	Addresses map[common.Address]int
	Slots     []map[common.Hash]struct{}
}

// 目前看来这个函数没啥用，具体实现在本地文件中
// UserToStateDBAccessList1 用户 -> 状态树转化，不是简单的转化，而是加上了from，to和precompiled，将User的AccessList转为stateDB的AccessList的函数，接收User的访问列表，返回一个stateDB的访问列表
func UserToStateDBAccessList1(rules params.Rules, sender, coinbase common.Address, dst *common.Address, precompiles []common.Address, list types.AccessList) interface{} {
	return nil
}

// 目前看来这个函数没啥用，具体实现在本地文件中
// UserToStateDBAccessList2 不再画蛇添足，只是按照数据进行转化，list是传入的用户的AccessList
func UserToStateDBAccessList2(list types.AccessList) interface{} {
	return nil
}

// NewJudgeAccessList 新建一个JudgeAccessList类型对象
func NewJudgeAccessList() *JudgeAccessList {
	return &JudgeAccessList{
		Addresses: nil,
		Slots:     nil,
	}
}

// JudgeAccessListIsAddressExce 判断指定地址是否在JudgeAccessList中，返回true表示存在，返回false表示不存在
func (j JudgeAccessList) JudgeAccessListIsAddressExce(address common.Address) bool {
	_, result := j.Addresses[address]
	return result
}

// JudgeAccessListAddAddress JudgeAccessList添加Address方法，添加后直接将Address的标志位设置为-1
func (j *JudgeAccessList) JudgeAccessListAddAddress(address common.Address) bool {
	if _, result := j.Addresses[address]; result { // result表示能否从指定key找到对应的value，返回int（slot的数组项序号）和bool
		return false // 该地址已经存在
	}
	j.Addresses[address] = -1 // Address对应的标志位设置为-1
	return true
}

// JudgeAccessListAddSlot JudgeAccessList添加Address对应的slot方法
func (j *JudgeAccessList) JudgeAccessListAddSlot(address common.Address, slot common.Hash) (addrChange bool, slotChange bool) {
	index, result := j.Addresses[address]
	if !result || index == -1 { // 当address不存在或者address存在但是没有slot（index = -1）
		j.Addresses[address] = len(j.Slots) // 新建一个address-int的kv对或修改已经存在kv对的int值为最新位置（j.slots数组长度）
		slotmap := map[common.Hash]struct{}{slot: {}}
		j.Slots = append(j.Slots, slotmap) // 加入slot数组
		return !result, true
	}
	slotmap := j.Slots[index]
	if _, ok := slotmap[slot]; !ok { // address存在，slot也不为空，但是对应的slot不存在
		slotmap[slot] = struct{}{}
		// Journal add slot change
		return false, true
	}
	// No changes required address与slot都存在
	return false, false
}

// GetTrueAccessList 得到当前操作实际访问到的AccessList，类型归类为*JudgeAccessList，表明可以对调用数据进行修改
func (j *JudgeAccessList) GetTrueAccessList(op OpCode, scope *ScopeContext) {
	//a := NewJudgeAccessList().GetTrueAccessList
	stack := scope.Stack // scope ScopeContext包含每个调用的东西，比如堆栈和内存
	stackData := stack.Data()
	stackLen := len(stackData)
	if (op == SLOAD || op == SSTORE) && stackLen >= 1 {
		slot := common.Hash(stackData[stackLen-1].Bytes32())
		//a.list.addSlot(scope.Contract.Address(), slot)
		j.JudgeAccessListAddSlot(scope.Contract.Address(), slot)
	}
	if (op == EXTCODECOPY || op == EXTCODEHASH || op == EXTCODESIZE || op == BALANCE || op == SELFDESTRUCT) && stackLen >= 1 {
		addr := common.Address(stackData[stackLen-1].Bytes20())
		if ok := j.JudgeAccessListIsAddressExce(addr); !ok {
			j.JudgeAccessListAddAddress(addr)
		}
	}
	if (op == DELEGATECALL || op == CALL || op == STATICCALL || op == CALLCODE) && stackLen >= 5 {
		addr := common.Address(stackData[stackLen-2].Bytes20())
		if ok := j.JudgeAccessListIsAddressExce(addr); !ok {
			j.JudgeAccessListAddAddress(addr)
		}
	}
	if op == CREATE || op == CREATE2 {
		// TODO: 是否也会访问和修改地址
	}
}

// ConflictDetection 冲突检测函数，检测stateDB中的AccessList和真实的JudgeAccessList之间有没有不一样的部分，如果出现了不一样，就需要将该交易放到串行队列中
// 返回值：是否发生冲突（false表示AL不一样，true表示没有AL一样），发生冲突项是否有Slot，地址是多少，Slot是多少。注意，后三项返回值只需要在result为false才有意义
func (j *JudgeAccessList) ConflictDetection(stateAL *state.AccessList) (result bool, haveSlot bool, address common.Address, slot []common.Hash) {
	for key, value := range j.Addresses {
		// 不存在slot，address相同则发生冲突
		if value == -1 {
			result := stateAL.ContainsAddress(key) // false则表示出错，超出了范围
			if !result {
				return false, false, key, []common.Hash{}
			}
		} else {
			// 存在slot，address和slot都相同则发生冲突
			// 先判断地址是否存在，不存在直接报错
			result := stateAL.ContainsAddress(key) // false则表示出错，超出了范围
			var temp []common.Hash
			for key, _ := range j.Slots[value] {
				temp = append(temp, key) // temp中存储了对应的slot值
			}
			if !result { // 地址不存在
				return false, true, key, temp
			} else {
				// 地址存在，判断slot
				// 拿到用户表中对应address的slot
				var tempUser []common.Hash
				for key, _ := range stateAL.Slots[stateAL.Addresses[key]] { // stateAL.Addresses[key]是stateDB的AccessList保存地址对应的slot序号
					tempUser = append(temp, key) // temp中存储了对应的slot值
				}
				// 判断slot是否一样
				if len(temp) != len(tempUser) { // 长度不一样
					return false, true, key, temp
				}
				for _, a := range tempUser {
					isSlotEqual := false
					for i := 0; i < len(temp); i++ {
						if bytes.Compare(a[:], temp[i][:]) == 0 {
							// 存在相等的
							isSlotEqual = true
							break
						}
					}
					if !isSlotEqual {
						// 不存在相等的slot，访问了AccessList列表之外的地址信息
						return false, true, key, temp
					}
				}
			}
		}
	}
	return true, true, common.Address{}, []common.Hash{} // 没有出现冲突，返回true，此时只需要关心result的结果就行，后面三个结果不需要关心
}

// CombineTrueAccessList 构造完整的AccessList函数，将传入的部分AccessList合并到总的AccessList中
func (j *JudgeAccessList) CombineTrueAccessList(tempAL *JudgeAccessList) bool {
	for key, value := range tempAL.Addresses {
		// 添加address
		j.JudgeAccessListAddAddress(key)
		// 添加slot
		if value != -1 {
			for slotkey, _ := range tempAL.Slots[value] {
				j.JudgeAccessListAddSlot(key, slotkey)
			}
		} else {
			// 只有address没有slot
		}
	}
	return true
}
