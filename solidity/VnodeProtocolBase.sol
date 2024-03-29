pragma solidity ^0.4.11;

/**
 * @title VnodeProtocolBase.sol
 * @author David Chen
 * @dev 
 * Subchain definition for application.
 * All the operations are done using the smallest and indivisible token unit,
 * just as on Ethereum all the operations are done in wei.
 */
contract SysContract {
    function delayedSend(uint256 _blk, address _to, uint256 _value, bool bonded) public returns (bool success);
}


contract VnodeProtocolBase {
    enum VnodeStatus { notRegistered, performing, withdrawPending, initialPending, withdrawDone, inactive }

    struct Vnode {
        address from; //address as id
        uint256 bond;   // value
        uint256 state; // one of VnodeStatus
        uint256 registerBlock;
        uint256 withdrawBlock;
        string link;
    }

    mapping(address => uint256) public vnodeList;
    Vnode[] public vnodeStore;

    uint256 public vnodeCount;
    uint256 public bondMin;
    address public owner;
    mapping(address => address[]) public outageReportList;

    uint256 public constant PEDNING_BLOCK_DELAY = 50; // 8 minutes
    uint256 public constant WITHDRAW_BLOCK_DELAY = 8640; // one day, given 10s block rate
 
    //events
    //event Registered(address vnode);
    // event UnRegistered(address sender);

    //constructor
    function VnodeProtocolBase(uint256 bmin) public {
        vnodeCount = 0;
        bondMin = bmin;
        //register a dummy one
        Vnode memory nd;
        nd.from = address(0);
        nd.bond = 0;
        nd.state = uint256(VnodeStatus.performing);
        nd.registerBlock = block.number + PEDNING_BLOCK_DELAY;
        nd.withdrawBlock = 2 ** 256 - 1;
        nd.link = "";
        
        vnodeStore.push(nd);
        vnodeCount++;

        owner = msg.sender;
    }

    function() public payable {  
        revert();
    }

    // register for vnode
    function register(address vnode, string link) public payable returns (bool) {
        //already registered or not enough bond
        require( vnodeList[vnode] == 0 && msg.value >= bondMin*10**18 );

        Vnode memory nd;
        nd.from = vnode;
        nd.bond = msg.value;
        nd.state = uint256(VnodeStatus.performing);
        nd.registerBlock = block.number + PEDNING_BLOCK_DELAY;
        nd.withdrawBlock = 2 ** 256 - 1;
        nd.link = link;
        
        vnodeStore.push(nd);
        vnodeList[vnode] = vnodeCount;
        vnodeCount++;
        return true;
    }

    // withdrawRequest for vnode
    function withdrawRequest() public returns (bool success) {
        //only can withdraw when active
        require(vnodeList[msg.sender] > 0 );
        uint256 index = vnodeList[msg.sender];
        require(vnodeStore[index].from == msg.sender);
        require(vnodeStore[index].state == uint256(VnodeStatus.performing));

        vnodeStore[index].withdrawBlock = block.number;
        vnodeStore[index].state = uint256(VnodeStatus.withdrawPending);

        //UnRegistered(msg.sender);
        return true;
    }

    function withdraw() public {
        require( vnodeList[msg.sender] > 0 );
        uint256 index = vnodeList[msg.sender];
        require( vnodeStore[index].from == msg.sender);

        if (
            vnodeStore[index].state == uint256(VnodeStatus.withdrawPending)
            && block.number > (vnodeStore[index].withdrawBlock + WITHDRAW_BLOCK_DELAY)
        ) {
            uint256 value = vnodeStore[index].bond;
            //replace with last one
            vnodeCount--;
			
			//update vnodeList
            vnodeList[vnodeStore[vnodeCount].from] = index;
			delete vnodeList[vnodeStore[index].from];
			
			 //replace with last one
            vnodeStore[index].from = vnodeStore[vnodeCount].from;
            vnodeStore[index].bond = vnodeStore[vnodeCount].bond;
	    vnodeStore[index].state = vnodeStore[vnodeCount].state;
            vnodeStore[index].registerBlock = vnodeStore[vnodeCount].registerBlock;
            vnodeStore[index].withdrawBlock = vnodeStore[vnodeCount].withdrawBlock;
            vnodeStore[index].link = vnodeStore[vnodeCount].link;
            delete vnodeStore[vnodeCount];

            // update length
	    vnodeStore.length--;

            //refund to sender
            msg.sender.transfer(value);
        }
    }

    function isPerforming(address _addr) public view returns (bool res) {
        if(vnodeList[_addr] == 0 ) {
            return false;
        }
        return (vnodeStore[vnodeList[_addr]].state == uint256(VnodeStatus.performing) && 
        vnodeStore[vnodeList[_addr]].registerBlock < block.number);
    }

    function pickRandomVnode(uint256 randness) public view returns (string target) {
        //com        
        if (vnodeCount < 2 ) {
            return "";
        } 

        uint256 index = randness%vnodeCount;
        //skip dummy
        if(index ==0 ){
            index++;
        }
        if( isPerforming(vnodeStore[index].from) ) {
            return vnodeStore[index].link; 
        }

        return  "";
    }

    //report one vnode is outage. limit to 5, no duplicate report from one sender
    function reportOutage(address vnode ) public {
        if( outageReportList[vnode].length < 5 ) {
            //check if reported by this sender already
            for( uint256 i=0; i<outageReportList[vnode].length; i++) {
                if( outageReportList[vnode][i] == msg.sender ) {
                    return;
                }
            }
            outageReportList[vnode].push(msg.sender);
        }
    }

    //sweep only n vnodes at a time
    function sweepOutage(uint256 level, uint256 startpos, uint256 count) public {
        require(msg.sender == owner);
        require(level > 0 && level <= 5);
        require(startpos > 0 && startpos < vnodeCount);

        //check endpos 
        uint256 endpos = startpos + count - 1;
        if(endpos >= vnodeCount ) {
            endpos = vnodeCount - 1;
        }

        //check each node
        uint256 i = 0;
        for( i=endpos; i>=startpos; i-- ) {
            //if in outage list
            if( outageReportList[vnodeStore[i].from].length >= level ) {
                delete outageReportList[vnodeStore[i].from];

                //remove from list
                //replace with last one
                vnodeCount--;
				
		//update vnodeList
		vnodeList[vnodeStore[vnodeCount].from] = i;
		delete vnodeList[vnodeStore[i].from];
				
		//replace with last one
                vnodeStore[i].from = vnodeStore[vnodeCount].from;
                vnodeStore[i].bond = vnodeStore[vnodeCount].bond;
		vnodeStore[i].state = vnodeStore[vnodeCount].state;
                vnodeStore[i].registerBlock = vnodeStore[vnodeCount].registerBlock;
                vnodeStore[i].withdrawBlock = vnodeStore[vnodeCount].withdrawBlock;
                vnodeStore[i].link = vnodeStore[vnodeCount].link;
                delete vnodeStore[vnodeCount];

                // update length
		vnodeStore.length--;


            }
        }

    }

}
